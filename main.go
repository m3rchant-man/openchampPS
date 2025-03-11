package main

import (
	"bufio"
	"context"
	"database/sql"
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
	"github.com/jackc/pgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Configuration
const (
	listenAddr      = ":8080"
	readBufferSize  = 1024
	writeBufferSize = 1024
	maxConnections  = 1000000 // 1 million connections
	baseGamePort    = 7000    // Starting port for game servers
	maxGamePort     = 8000    // Maximum port for game servers
)

const dbConnString = "postgres://postgres:password@localhost:5432/openchamp"

// PortManager manages game server port allocation
type PortManager struct {
	mutex       sync.Mutex
	usedPorts   map[int]bool
	basePort    int
	maxPort     int
	currentPort int
}

// NewPortManager creates a new port manager
func NewPortManager(basePort, maxPort int) *PortManager {
	return &PortManager{
		usedPorts:   make(map[int]bool),
		basePort:    basePort,
		maxPort:     maxPort,
		currentPort: basePort,
	}
}

// GetPort allocates a new port
func (pm *PortManager) GetPort() (int, error) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	// Try to find an available port starting from currentPort
	startPort := pm.currentPort
	for {
		// Check if port is already marked as used
		if !pm.usedPorts[pm.currentPort] {
			// Verify port is actually available at OS level
			if isPortAvailable(pm.currentPort) {
				pm.usedPorts[pm.currentPort] = true
				allocatedPort := pm.currentPort

				// Increment for next allocation
				pm.currentPort++
				if pm.currentPort > pm.maxPort {
					pm.currentPort = pm.basePort
				}

				return allocatedPort, nil
			}
		}

		// Move to next port
		pm.currentPort++
		if pm.currentPort > pm.maxPort {
			pm.currentPort = pm.basePort
		}

		// If we've checked all ports and found none, return error
		if pm.currentPort == startPort {
			return 0, fmt.Errorf("no available ports between %d and %d", pm.basePort, pm.maxPort)
		}
	}
}

// ReleasePort marks a port as no longer in use
func (pm *PortManager) ReleasePort(port int) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	delete(pm.usedPorts, port)
	log.Printf("Port %d released", port)
}

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

	// Authentication fields
	authenticated bool
	authToken     string
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
	Cmd       *exec.Cmd // Store the command for proper process management
}

var (
	activeMatches = make(map[string]*Match)
	matchesMutex  sync.RWMutex
	portManager   *PortManager
)

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
			// Initially assign a temporary guest username
			client.username = genRandomUsername()
			client.authenticated = false

			manager.mutex.Lock()
			manager.clients[client] = true
			manager.mutex.Unlock()
			log.Printf("Client connected: %s, Total clients: %d", client.id, len(manager.clients))

			// Send a connection acknowledgment with temporary username
			client.send <- []byte(`{"type": "connection_established", "payload": {"temp_username": "` + client.username + `", "auth_required": true}}`)

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

	// Authentication and registration messages are always allowed
	if msg.Type == "login" || msg.Type == "token_auth" || msg.Type == "register" {
		switch msg.Type {
		case "login", "token_auth":
			client.handleAuthentication(msg)
		case "register":
			client.handleRegistration(msg)
		}
		return
	}

	// For other message types, check if authenticated
	if !client.authenticated {
		// Reject non-authenticated requests
		response := map[string]interface{}{
			"type": "error",
			"payload": map[string]interface{}{
				"message": "Authentication required",
				"code":    "AUTH_REQUIRED",
			},
		}
		responseJSON, _ := json.Marshal(response)
		client.send <- responseJSON
		return
	}

	// Process different message types for authenticated users
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
		client.manager.mutex.Lock()
		client.manager.queue = append(client.manager.queue, client)
		client.manager.mutex.Unlock()
		client.send <- []byte(`{"type": "queued"}`)

	case "dequeue":
		client.manager.mutex.Lock()
		for i, c := range client.manager.queue {
			if c == client {
				client.manager.queue = append(client.manager.queue[:i], client.manager.queue[i+1:]...)
				break
			}
		}
		client.manager.mutex.Unlock()
		client.send <- []byte(`{"type": "dequeued"}`)

	// Set Functions
	case "set_username":
		// Authenticated users can only change their display name, not their actual username
		var newDisplayName string
		if err := json.Unmarshal(msg.Payload, &newDisplayName); err == nil {
			client.send <- []byte(`{"type": "display_name_updated", "payload": "` + newDisplayName + `"}`)
		}

	case "logout":
		// Handle logout
		client.authenticated = false
		client.authToken = ""
		client.username = genRandomUsername()
		client.send <- []byte(`{"type": "logged_out"}`)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// Handle authentication requests
func (client *Client) handleAuthentication(msg Message) {
	switch msg.Type {
	case "login":
		// Username/password authentication
		var credentials struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}

		if err := json.Unmarshal(msg.Payload, &credentials); err != nil {
			log.Printf("Error parsing login credentials: %v", err)
			client.sendAuthError("Invalid login format")
			return
		}

		// Validate credentials against database
		authenticated, token, err := client.validateCredentials(credentials.Username, credentials.Password)
		if err != nil || !authenticated {
			client.sendAuthError("Invalid username or password")
			return
		}

		// Authentication successful
		client.completeAuthentication(credentials.Username, token)

	case "token_auth":
		// Token-based authentication
		var tokenAuth struct {
			Token string `json:"token"`
		}

		if err := json.Unmarshal(msg.Payload, &tokenAuth); err != nil {
			log.Printf("Error parsing token auth: %v", err)
			client.sendAuthError("Invalid token format")
			return
		}

		// Extract client's real IP address from connection
		clientIP := client.getClientIP()

		// Validate token against database
		username, valid, err := client.validateToken(tokenAuth.Token, clientIP)
		if err != nil || !valid {
			client.sendAuthError("Invalid or expired token")
			return
		}

		// Authentication successful
		client.completeAuthentication(username, tokenAuth.Token)
	}
}

// Send authentication error response
func (client *Client) sendAuthError(message string) {
	response := map[string]interface{}{
		"type": "auth_error",
		"payload": map[string]interface{}{
			"message": message,
		},
	}
	responseJSON, _ := json.Marshal(response)
	client.send <- responseJSON
}

// Handle user registration via WebSocket
func (client *Client) handleRegistration(msg Message) {
	var registration struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Email    string `json:"email"`
	}

	if err := json.Unmarshal(msg.Payload, &registration); err != nil {
		log.Printf("Error parsing registration data: %v", err)
		client.sendRegistrationError("Invalid registration format")
		return
	}

	// Validate input
	if len(registration.Username) < 3 {
		client.sendRegistrationError("Username must be at least 3 characters")
		return
	}

	if len(registration.Password) < 6 {
		client.sendRegistrationError("Password must be at least 6 characters")
		return
	}

	// Check if username already exists
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var exists bool
	err := client.dbPool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)",
		registration.Username).Scan(&exists)

	if err != nil {
		log.Printf("Database error during registration: %v", err)
		client.sendRegistrationError("Registration failed due to a server error")
		return
	}

	if exists {
		client.sendRegistrationError("Username already exists")
		return
	}

	// Check if email already exists (if provided)
	if registration.Email != "" {
		err = client.dbPool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)",
			registration.Email).Scan(&exists)

		if err != nil {
			log.Printf("Database error during registration: %v", err)
			client.sendRegistrationError("Registration failed due to a server error")
			return
		}

		if exists {
			client.sendRegistrationError("Email already registered")
			return
		}
	}

	// TODO: Hash the password in production
	passwordHash := registration.Password // Placeholder for demo

	// Insert user into database
	_, err = client.dbPool.Exec(ctx,
		"INSERT INTO users (username, password_hash, email) VALUES ($1, $2, $3)",
		registration.Username, passwordHash, registration.Email)

	if err != nil {
		log.Printf("Error inserting new user: %v", err)
		client.sendRegistrationError("Registration failed due to a server error")
		return
	}

	// Generate token for automatic login
	token := uuid.New().String()

	// Get the user ID for the token
	var userID int
	err = client.dbPool.QueryRow(ctx,
		"SELECT id FROM users WHERE username = $1",
		registration.Username).Scan(&userID)

	if err != nil {
		log.Printf("Error retrieving new user ID: %v", err)
		// Registration was successful, but auto-login failed
		client.sendRegistrationSuccess(false, "", "")
		return
	}

	// Get client's real IP
	clientIP := client.getClientIP()

	// Store token with IP
	_, err = client.dbPool.Exec(ctx,
		"INSERT INTO auth_tokens (user_id, token, ip_address, created_at, expires_at) VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '7 days')",
		userID, token, clientIP)

	if err != nil {
		log.Printf("Error creating token for new user: %v", err)
		// Registration was successful, but auto-login failed
		client.sendRegistrationSuccess(false, "", "")
		return
	}

	// Registration and auto-login successful
	client.username = registration.Username
	client.authenticated = true
	client.authToken = token

	// Send success response with auto-login token
	client.sendRegistrationSuccess(true, registration.Username, token)

	log.Printf("New user registered and authenticated: %s", registration.Username)
}

// Send registration error response
func (client *Client) sendRegistrationError(message string) {
	response := map[string]interface{}{
		"type": "register_error",
		"payload": map[string]interface{}{
			"message": message,
		},
	}
	responseJSON, _ := json.Marshal(response)
	client.send <- responseJSON
}

// Send registration success response
func (client *Client) sendRegistrationSuccess(autoLogin bool, username, token string) {
	payload := map[string]interface{}{
		"success":   true,
		"message":   "Registration successful",
		"autoLogin": autoLogin,
	}

	// Include login info if auto-login succeeded
	if autoLogin {
		payload["username"] = username
		payload["token"] = token
	}

	response := map[string]interface{}{
		"type":    "register_success",
		"payload": payload,
	}

	responseJSON, _ := json.Marshal(response)
	client.send <- responseJSON
}

// Complete the authentication process
func (client *Client) completeAuthentication(username string, token string) {
	client.authenticated = true
	client.username = username
	client.authToken = token

	// Send successful authentication response
	response := map[string]interface{}{
		"type": "auth_success",
		"payload": map[string]interface{}{
			"username": username,
			"token":    token,
		},
	}
	responseJSON, _ := json.Marshal(response)
	client.send <- responseJSON

	log.Printf("Client authenticated: %s as %s", client.id, username)
}

// Validate username and password against database
func (client *Client) validateCredentials(username, password string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		userID       int
		passwordHash string
	)

	// Query the database for user credentials
	// Note: In production, you should use proper password hashing like bcrypt
	err := client.dbPool.QueryRow(ctx,
		"SELECT id, password_hash FROM users WHERE username = $1",
		username).Scan(&userID, &passwordHash)

	if err != nil {
		if err == pgx.ErrNoRows {
			return false, "", nil // User not found
		}
		return false, "", err // Database error
	}

	// TODO: Use proper password verification (bcrypt, etc.)
	// This is just a placeholder - in production use secure comparison
	if passwordHash != password {
		return false, "", nil // Password doesn't match
	}

	// Generate a new auth token
	token := uuid.New().String()

	// Get client's real IP
	clientIP := client.getClientIP()

	// Store the token in the database with IP
	_, err = client.dbPool.Exec(ctx,
		"INSERT INTO auth_tokens (user_id, token, ip_address, created_at, expires_at) VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '7 days')",
		userID, token, clientIP)

	if err != nil {
		return false, "", err
	}

	return true, token, nil
}

// Get the client's real IP address
func (client *Client) getClientIP() string {
	// Extract IP from WebSocket connection
	// First, get the remote address from the connection
	remoteAddr := client.conn.RemoteAddr().String()

	// Extract just the IP part (remove port)
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Fallback to full address if we can't split it
		return remoteAddr
	}

	return ip
}

// Validate token and IP against database
func (client *Client) validateToken(token, clientIP string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		username string
		storedIP sql.NullString
		tokenID  int
	)

	// Query the database to validate the token
	err := client.dbPool.QueryRow(ctx,
		`SELECT t.id, u.username, t.ip_address
		FROM auth_tokens t
		JOIN users u ON t.user_id = u.id
		WHERE t.token = $1 
		AND t.expires_at > NOW()`,
		token).Scan(&tokenID, &username, &storedIP)

	if err != nil {
		if err == pgx.ErrNoRows {
			return "", false, nil // Token not found or expired
		}
		return "", false, err // Database error
	}

	// Check IP restrictions if a previous IP is stored
	if storedIP.Valid && storedIP.String != "" {
		// If this is a different IP than previously used with this token,
		// we can either reject it or implement additional security checks
		if storedIP.String != clientIP {
			log.Printf("Warning: Token used from new IP. Original: %s, Current: %s",
				storedIP.String, clientIP)

			// Depending on security requirements, you might want to:
			// 1. Reject the attempt (uncomment the next line)
			// return "", false, nil

			// 2. Allow it but track the new IP
			// 3. Require additional verification
			// 4. Rate limit new IP logins
		}
	}

	// Update the token's last_used_at timestamp and IP
	_, err = client.dbPool.Exec(ctx,
		`UPDATE auth_tokens 
		SET last_used_at = NOW(), 
		    ip_address = $1
		WHERE id = $2`,
		clientIP, tokenID)

	if err != nil {
		log.Printf("Error updating token usage: %v", err)
		// Non-critical error, we can continue
	}

	return username, true, nil
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
	if runtime.GOOS == "linux" {
		var rLimit syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
			log.Println("Error getting rlimit:", err)
		} else {
			rLimit.Cur = rLimit.Max
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
				log.Println("Error setting rlimit:", err)
			}
		}
	}
}

// Generate a random username (until login system is implemented)
func genRandomUsername() string {
	return "user_" + strconv.Itoa(rand.Intn(1000))
}

// Check if there are enough players in the queue to start a match
func checkQueue(manager *ClientManager) {
	var playersPerMatch = 1

	manager.mutex.Lock()
	queueLength := len(manager.queue)
	manager.mutex.Unlock()

	if queueLength >= playersPerMatch {
		manager.mutex.Lock()
		players := manager.queue[:playersPerMatch]
		manager.queue = manager.queue[playersPerMatch:]
		manager.mutex.Unlock()

		log.Println("Starting match with", playersPerMatch, "players")
		go startMatch(players) // Run in goroutine to not block queue processing
	}
}

// Start a match
func startMatch(players []*Client) {
	// Generate a unique match ID using UUID
	matchID := uuid.New().String()

	// Get a port from the port manager
	port, err := portManager.GetPort()
	if err != nil {
		log.Printf("Failed to allocate port for match %s: %v", matchID, err)
		notifyMatchFailure(players)
		return
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
		portManager.ReleasePort(port)
		notifyMatchFailure(players)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Error creating stderr pipe for match %s: %v", matchID, err)
		portManager.ReleasePort(port)
		notifyMatchFailure(players)
		return
	}

	// Start the server process
	if err := cmd.Start(); err != nil {
		log.Printf("Error starting game server for match %s: %v", matchID, err)
		portManager.ReleasePort(port)
		notifyMatchFailure(players)
		return
	}

	// Log server output
	go logServerOutput(stdout, stderr, matchID)

	// Store the match data
	match := &Match{
		ID:        matchID,
		Port:      port,
		Process:   cmd.Process,
		Players:   players,
		StartTime: time.Now(),
		Cmd:       cmd,
	}

	matchesMutex.Lock()
	activeMatches[matchID] = match
	matchesMutex.Unlock()

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
	go monitorMatchProcess(cmd, matchID, port)
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
func monitorMatchProcess(cmd *exec.Cmd, matchID string, port int) {
	err := cmd.Wait()

	// Process terminated
	log.Printf("Match %s terminated: %v", matchID, err)

	// Release the port back to the port manager
	portManager.ReleasePort(port)

	// Clean up the match data
	matchesMutex.Lock()
	match, exists := activeMatches[matchID]
	matchesMutex.Unlock()

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
		matchesMutex.Lock()
		delete(activeMatches, matchID)
		matchesMutex.Unlock()
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

// Create necessary database tables if they don't exist
func setupDatabase(dbPool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create users table
	_, err := dbPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			username VARCHAR(50) UNIQUE NOT NULL,
			password_hash VARCHAR(255) NOT NULL,
			email VARCHAR(255) UNIQUE,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			last_login TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create users table: %w", err)
	}

	// Create auth_tokens table
	_, err = dbPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS auth_tokens (
			id SERIAL PRIMARY KEY,
			user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
			token VARCHAR(255) UNIQUE NOT NULL,
			ip_address VARCHAR(50),
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			expires_at TIMESTAMP NOT NULL,
			last_used_at TIMESTAMP,
			is_revoked BOOLEAN NOT NULL DEFAULT FALSE
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create auth_tokens table: %w", err)
	}

	// Create indexes for faster lookups
	_, err = dbPool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_token ON auth_tokens(token);
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_user_id ON auth_tokens(user_id);
	`)
	if err != nil {
		return fmt.Errorf("failed to create indexes: %w", err)
	}

	log.Println("Database tables initialized successfully")
	return nil
}

func main() {
	// Set up the port manager
	portManager = NewPortManager(baseGamePort, maxGamePort)

	// Initialize the client manager
	manager := NewClientManager()
	go manager.Start()

	// Initialize the database connection pool
	dbPool, err := initDBPool()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer dbPool.Close()

	// Set up database tables
	if err := setupDatabase(dbPool); err != nil {
		log.Printf("Warning: Database setup issue: %v", err)
	}

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

	// User registration endpoint
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var registration struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Email    string `json:"email"`
		}

		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate input
		if len(registration.Username) < 3 || len(registration.Password) < 6 {
			http.Error(w, "Username must be at least 3 characters and password at least 6 characters", http.StatusBadRequest)
			return
		}

		// TODO: Hash the password with bcrypt in production
		passwordHash := registration.Password // Placeholder for demo

		// Insert user into database
		ctx := r.Context()
		_, err := dbPool.Exec(ctx,
			"INSERT INTO users (username, password_hash, email) VALUES ($1, $2, $3)",
			registration.Username, passwordHash, registration.Email)

		if err != nil {
			log.Printf("Error registering user: %v", err)
			http.Error(w, "Username or email already exists", http.StatusConflict)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "success",
			"message": "User registered successfully",
		})
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

	// Stop all running game servers
	matchesMutex.Lock()
	for id, match := range activeMatches {
		log.Printf("Stopping game server for match %s...", id)
		if match.Process != nil {
			if err := match.Process.Kill(); err != nil {
				log.Printf("Error killing process for match %s: %v", id, err)
			}
		}
	}
	matchesMutex.Unlock()

	// Shutdown the server
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server gracefully stopped")
}
