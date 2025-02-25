package main

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Configuration
const (
	listenAddr      = ":8080"
	readBufferSize  = 1024
	writeBufferSize = 1024
	maxConnections  = 1000000 // 1 million connections
)
const dbConnString = "postgres://postgres:password@localhost:5432/openchamp"

// ClientManager manages active WebSocket clients
type ClientManager struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
}

// Client represents a connected WebSocket client
type Client struct {
	id      string
	conn    *websocket.Conn
	send    chan []byte
	manager *ClientManager
	dbPool  *pgxpool.Pool
}

// Message represents a WebSocket message structure
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Configure WebSocket upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  readBufferSize,
	WriteBufferSize: writeBufferSize,
	CheckOrigin: func(r *http.Request) bool {
		return true // In production, implement proper origin checks
	},
}

// Initialize the client manager
func NewClientManager() *ClientManager {
	return &ClientManager{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Start the client manager
func (manager *ClientManager) Start() {
	for {
		select {
		case client := <-manager.register:
			manager.mutex.Lock()
			manager.clients[client] = true
			manager.mutex.Unlock()
			log.Printf("Client connected: %s, Total clients: %d", client.id, len(manager.clients))
			client.send <- []byte(`{"type": "user_assigned", "name": ` + genRandomUsername() + `}`)

		case client := <-manager.unregister:
			manager.mutex.Lock()
			if _, ok := manager.clients[client]; ok {
				delete(manager.clients, client)
				close(client.send)
			}
			manager.mutex.Unlock()
			log.Printf("Client disconnected: %s, Total clients: %d", client.id, len(manager.clients))

		case message := <-manager.broadcast:
			manager.mutex.RLock()
			for client := range manager.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(manager.clients, client)
				}
			}
			manager.mutex.RUnlock()
		}
	}
}

// Handle WebSocket connections
func (manager *ClientManager) HandleWebSocket(w http.ResponseWriter, r *http.Request, dbPool *pgxpool.Pool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading connection:", err)
		return
	}

	client := &Client{
		id:      r.RemoteAddr,
		conn:    conn,
		send:    make(chan []byte, 256),
		manager: manager,
		dbPool:  dbPool,
	}

	manager.register <- client

	go client.readPump()
	go client.writePump()
}

// Read messages from the WebSocket
func (client *Client) readPump() {
	defer func() {
		client.manager.unregister <- client
		client.conn.Close()
	}()

	client.conn.SetReadLimit(int64(readBufferSize))
	client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.conn.SetPongHandler(func(string) error {
		client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading message: %v", err)
			}
			break
		}

		// Process the message
		go client.processMessage(message)
	}
}

// Write messages to the WebSocket
func (client *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		client.conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// The manager closed the channel
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued messages to the current WebSocket message
			n := len(client.send)
			for i := 0; i < n; i++ {
				w.Write(<-client.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Process incoming messages
func (client *Client) processMessage(data []byte) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Error parsing message: %v", err)
		return
	}

	// Process different message types
	switch msg.Type {
	case "count":
		response := map[string]interface{}{
			"type":  "count_response",
			"count": len(client.manager.clients),
		}
		responseJSON, _ := json.Marshal(response)
		client.send <- responseJSON
	case "query":
		client.handleDatabaseQuery(msg.Payload)
	case "broadcast":
		client.manager.broadcast <- data
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// Handle database queries
func (client *Client) handleDatabaseQuery(payload json.RawMessage) {
	var query struct {
		SQL    string        `json:"sql"`
		Params []interface{} `json:"params"`
	}

	if err := json.Unmarshal(payload, &query); err != nil {
		log.Printf("Error parsing query: %v", err)
		return
	}

	// Execute query with connection pooling
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := client.dbPool.Query(ctx, query.SQL, query.Params...)
	if err != nil {
		response := map[string]interface{}{
			"type":  "db_response",
			"error": err.Error(),
		}
		responseJSON, _ := json.Marshal(response)
		client.send <- responseJSON
		return
	}
	defer rows.Close()

	// Process query results
	var results []map[string]interface{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}

		// Get column descriptions for field names
		fields := rows.FieldDescriptions()
		result := make(map[string]interface{})

		for i, value := range values {
			result[fields[i].Name] = value
		}

		results = append(results, result)
	}

	// Send query results back to client
	response := map[string]interface{}{
		"type":    "db_response",
		"results": results,
	}

	responseJSON, _ := json.Marshal(response)
	client.send <- responseJSON
}

// Initialize database connection pool
func initDBPool() (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dbConnString)
	if err != nil {
		return nil, err
	}

	// Configure the connection pool
	config.MaxConns = 100                      // Maximum number of connections
	config.MinConns = 10                       // Minimum idle connections
	config.MaxConnLifetime = 1 * time.Hour     // Maximum connection lifetime
	config.MaxConnIdleTime = 30 * time.Minute  // Maximum idle time
	config.HealthCheckPeriod = 1 * time.Minute // Health check period

	// Create the connection pool
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, err
	}

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}

	return pool, nil
}

// Configure server for high throughput (Uncomment when move to linux)
func configureServerForHighLoad(srv *http.Server) {

	// Increase the maximum number of open files on linux
	// if runtime.GOOS == "linux" {
	// 	var rLimit syscall.Rlimit
	// 	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
	// 		log.Println("Error getting rlimit:", err)
	// 	} else {
	// 		rLimit.Cur = rLimit.Max
	// 		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
	// 			log.Println("Error setting rlimit:", err)
	// 		}
	// 	}
	// }

}

func genRandomUsername() string {
	return "user_" + strconv.Itoa(rand.Intn(1000))
}

func main() {
	// Initialize the client manager
	manager := NewClientManager()
	go manager.Start()

	// Initialize the database connection pool
	dbPool, err := initDBPool()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer dbPool.Close()

	// Create router and handlers
	mux := http.NewServeMux()

	// WebSocket endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		manager.HandleWebSocket(w, r, dbPool)
	})

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Create server
	server := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	// Configure for high load
	configureServerForHighLoad(server)

	// Start the server in a goroutine
	go func() {
		log.Printf("Server starting on %s", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error starting server: %v", err)
		}
	}()

	// Set up graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Wait for shutdown signal
	<-stop

	log.Println("Shutting down server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown the server
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server gracefully stopped")
}
