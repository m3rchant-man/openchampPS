package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
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
	queue      []*Client
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
}

// Client represents a connected WebSocket client
type Client struct {
	id       string
	conn     *websocket.Conn
	send     chan []byte
	manager  *ClientManager
	dbPool   *pgxpool.Pool
	username string
}

// Message represents a WebSocket message structure
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Match represents an active game match
type Match struct {
	ID        string
	Port      int
	Process   *os.Process
	Players   []*Client
	StartTime time.Time
}

var activeMatches = make(map[string]*Match)

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
			client.username = genRandomUsername()
			manager.mutex.Lock()
			manager.clients[client] = true
			manager.mutex.Unlock()
			log.Printf("Client connected: %s, Total clients: %d", client.id, len(manager.clients))
			client.send <- []byte(`{"type": "user_assigned", "payload": "` + client.username + `"}`)

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
	// Lobby Functions
	case "count":
		response := map[string]interface{}{
			"type":    "player_count",
			"payload": len(client.manager.clients),
		}
		responseJSON, _ := json.Marshal(response)
		client.send <- responseJSON
	case "global_chat":
		chatMessage := map[string]interface{}{
			"type":    "global_chat",
			"payload": map[string]interface{}{"username": client.username, "message": string(msg.Payload)},
		}
		chatMessageJSON, _ := json.Marshal(chatMessage)
		client.manager.broadcast <- chatMessageJSON

	// Queue Functions
	case "queue":
		client.manager.queue = append(client.manager.queue, client)
		client.send <- []byte(`{"type": "queued"}`)

	case "dequeue":
		for i, c := range client.manager.queue {
			if c == client {
				client.manager.queue = append(client.manager.queue[:i], client.manager.queue[i+1:]...)
				break
			}
		}
		client.send <- []byte(`{"type": "dequeued"}`)

	// Set Functions
	case "set_username":
		client.username = string(msg.Payload)
		client.send <- []byte(`{"type": "user_assigned", "payload": ` + client.username + `}`)

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

// Generate a random username (until login system is implemented)
func genRandomUsername() string {
	return "user_" + strconv.Itoa(rand.Intn(1000))
}

// Check if there are enough players in the queue to start a match
func checkQueue(manager *ClientManager) {
	var playersPerMatch = 1

	if len(manager.queue) >= playersPerMatch {
		log.Println("Starting match with", playersPerMatch, "players")
		startMatch(manager.queue[:playersPerMatch])
		manager.queue = manager.queue[playersPerMatch:]
	}
}

// Start a match
func startMatch(players []*Client) {
	// Generate a unique match ID using UUID
	matchID := uuid.New().String()

	// Find an available port for the game server
	// Start with a base port and increment if needed
	basePort := 7000
	port := basePort

	// Check if port is available, increment if not
	for {
		if isPortAvailable(port) {
			break
		}
		port++
		// Safety check to avoid infinite loop
		if port > basePort+1000 {
			log.Printf("Failed to find available port for match %s", matchID)
			notifyMatchFailure(players)
			return
		}
	}

	// Determine which executable to run based on OS
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("./gameserver/OpenChamp.console.exe",
			"--port", strconv.Itoa(port),
			"--id", matchID)
	} else {
		cmd = exec.Command("./gameserver/OpenChamp.console.x86_64",
			"--port", strconv.Itoa(port),
			"--id", matchID)
	}

	// Set up pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Error creating stdout pipe for match %s: %v", matchID, err)
		notifyMatchFailure(players)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Error creating stderr pipe for match %s: %v", matchID, err)
		notifyMatchFailure(players)
		return
	}

	// Start the server process
	if err := cmd.Start(); err != nil {
		log.Printf("Error starting game server for match %s: %v", matchID, err)
		notifyMatchFailure(players)
		return
	}

	// Log server output
	go logServerOutput(stdout, stderr, matchID)

	// Store the process for later cleanup
	activeMatches[matchID] = &Match{
		ID:        matchID,
		Port:      port,
		Process:   cmd.Process,
		Players:   players,
		StartTime: time.Now(),
	}

	// Notify players of match start with server details
	matchInfo := map[string]interface{}{
		"type": "match_start",
		"payload": map[string]interface{}{
			"matchID": matchID,
			"port":    port,
		},
	}
	matchInfoJSON, _ := json.Marshal(matchInfo)

	for _, player := range players {
		player.send <- matchInfoJSON
	}

	log.Printf("Started match %s on port %d with %d players", matchID, port, len(players))

	// Monitor the process for termination
	go monitorMatchProcess(cmd, matchID)
}

// Check if a port is available
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Log server output
func logServerOutput(stdout, stderr io.ReadCloser, matchID string) {
	// Read and log stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			log.Printf("Match %s stdout: %s", matchID, scanner.Text())
		}
	}()

	// Read and log stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("Match %s stderr: %s", matchID, scanner.Text())
		}
	}()
}

// Monitor match process
func monitorMatchProcess(cmd *exec.Cmd, matchID string) {
	err := cmd.Wait()

	// Process terminated
	log.Printf("Match %s terminated: %v", matchID, err)

	// Clean up the match data
	match, exists := activeMatches[matchID]
	if exists {
		// Notify players if they're still connected
		matchEndJSON, _ := json.Marshal(map[string]interface{}{
			"type":    "match_end",
			"matchID": matchID,
		})

		for _, player := range match.Players {
			select {
			case player.send <- matchEndJSON:
			default:
				// Player might be disconnected already
			}
		}

		// Remove from active matches
		delete(activeMatches, matchID)
	}
}

// Notify players of match failure
func notifyMatchFailure(players []*Client) {
	failureJSON, _ := json.Marshal(map[string]interface{}{
		"type":  "match_error",
		"error": "Failed to start match server",
	})

	for _, player := range players {
		player.send <- failureJSON
	}
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

	// Start the queue processor in a goroutine
	go func() {
		// Check number of players in queue every 5 seconds
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			checkQueue(manager)
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
