package wsm

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"net"
	"net/http"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"openchamp/server/portmanager"
)

type ClientManager struct {
	clients    map[*Client]bool
	queue      []*Client
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
	shutdown   chan struct{}
}

func (manager *ClientManager) Start() {
	for {
		select {
		case client := <-manager.register:
			manager.mutex.Lock()
			manager.clients[client] = true
			manager.mutex.Unlock()

		case client := <-manager.unregister:
			if _, ok := manager.clients[client]; ok {
				manager.mutex.Lock()
				delete(manager.clients, client)
				close(client.send)
				manager.mutex.Unlock()
			}

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

		case <-manager.shutdown:
			manager.mutex.Lock()
			for client := range manager.clients {
				close(client.send)
				client.conn.Close()
				delete(manager.clients, client)
			}
			manager.mutex.Unlock()
			return
		}
	}
}

func (manager *ClientManager) Shutdown() {
	close(manager.shutdown)
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	
	manager.queue = nil
	
	log.Println("WebSocket manager shutdown complete")
}

func (manager *ClientManager) GetMutex() *sync.RWMutex {
	return &manager.mutex
}

func (manager *ClientManager) GetQueue() []*Client {
	return manager.queue
}

func (manager *ClientManager) SetQueue(newQueue []*Client) {
	manager.queue = newQueue
}

func (client *Client) Send() chan []byte {
	return client.send
}

type Client struct {
	id       string
	conn     *websocket.Conn
	send     chan []byte
	manager  *ClientManager
	dbPool   *pgxpool.Pool
	username string
	authenticated bool
	authToken     string
}

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

const (
	listenAddr      = ":8080"
	readBufferSize  = 1024
	writeBufferSize = 1024
	maxConnections  = 1000000
	baseGamePort    = 7000
	maxGamePort     = 8000
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  readBufferSize,
	WriteBufferSize: writeBufferSize,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		shutdown:   make(chan struct{}),
	}
}

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

/* === Websocket RWX === */

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

		go client.processMessage(message)
	}
}

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
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

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
	case "join_queue":
		if portmanager.Manager != nil {
			portmanager.Manager.AddToQueue(client.id, client.username, client)
			client.send <- []byte(`{"type": "queued"}`)
		}

	case "leave_queue":
		if portmanager.Manager != nil {
			if portmanager.Manager.RemoveFromQueue(client.id) {
				client.send <- []byte(`{"type": "dequeued"}`)
			}
		}

	// Set Functions
	case "set_username":
		var newDisplayName string
		if err := json.Unmarshal(msg.Payload, &newDisplayName); err == nil {
			client.send <- []byte(`{"type": "display_name_updated", "payload": "` + newDisplayName + `"}`)
			client.username = newDisplayName
		}

	case "logout":
		client.authenticated = false
		client.authToken = ""
		client.send <- []byte(`{"type": "logged_out"}`)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

/* === Authentication & Registration */

func (client *Client) completeAuthentication(username string, token string) {
	client.authenticated = true
	client.username = username
	client.authToken = token

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

		clientIP := client.getClientIP()

		username, valid, err := client.validateToken(tokenAuth.Token, clientIP)
		if err != nil || !valid {
			client.sendAuthError("Invalid or expired token")
			return
		}

		client.completeAuthentication(username, tokenAuth.Token)
	}
}

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

	if len(registration.Username) < 3 {
		client.sendRegistrationError("Username must be at least 3 characters")
		return
	}

	if len(registration.Password) < 6 {
		client.sendRegistrationError("Password must be at least 6 characters")
		return
	}

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

/* === EMAIL CHECKING (TURN ON IN PROD) === */
	// Check if email already exists (if provided)

	// if registration.Email != "" {
	// 	err = client.dbPool.QueryRow(ctx,
	// 		"SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)",
	// 		registration.Email).Scan(&exists)

	// 	if err != nil {
	// 		log.Printf("Database error during registration: %v", err)
	// 		client.sendRegistrationError("Registration failed due to a server error")
	// 		return
	// 	}

	// 	if exists {
	// 		client.sendRegistrationError("Email already registered")
	// 		return
	// 	}
	// }

	hashedPw, err := bcrypt.GenerateFromPassword([]byte(registration.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Error hashing password: %v", err)
		client.sendRegistrationError("Registration failed due to a server error")
		return
	}
	passwordHash := string(hashedPw)

	var userID int
	err = client.dbPool.QueryRow(ctx,
		"INSERT INTO users (username, password_hash, email) VALUES ($1, $2, $3) RETURNING id",
		registration.Username, passwordHash, registration.Email).Scan(&userID)

	if err != nil {
		log.Printf("Error inserting new user: %v", err)
		client.sendRegistrationError("Registration failed due to a server error")
		return
	}

	token := uuid.New().String()

	clientIP := client.getClientIP()

	_, err = client.dbPool.Exec(ctx,
		"INSERT INTO auth_tokens (user_id, token, ip_address, created_at, expires_at) VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '7 days')",
		userID, token, clientIP)

	if err != nil {
		log.Printf("Error creating token for new user: %v", err)
		client.sendRegistrationSuccess(false, "", "")
		return
	}

	client.username = registration.Username
	client.authenticated = true
	client.authToken = token

	client.sendRegistrationSuccess(true, registration.Username, token)

	log.Printf("New user registered and authenticated: %s", registration.Username)
}

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

func (client *Client) sendRegistrationSuccess(autoLogin bool, username, token string) {
	payload := map[string]interface{}{
		"success":   true,
		"message":   "Registration successful",
		"autoLogin": autoLogin,
	}

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

func (client *Client) validateCredentials(username, password string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		userID       int
		passwordHash string
	)

	err := client.dbPool.QueryRow(ctx,
		"SELECT id, password_hash FROM users WHERE username = $1",
		username).Scan(&userID, &passwordHash)

	if err != nil {
		if err == pgx.ErrNoRows {
			return false, "", nil
		}
		return false, "", err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		if err == bcrypt.ErrMismatchedHashAndPassword {
			return false, "", nil
		}
		return false, "", err
	}

	token := uuid.New().String()

	clientIP := client.getClientIP()

	_, err = client.dbPool.Exec(ctx,
		"INSERT INTO auth_tokens (user_id, token, ip_address, created_at, expires_at) VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '7 days')",
		userID, token, clientIP)

	if err != nil {
		return false, "", err
	}

	return true, token, nil
}

func (client *Client) validateToken(token, clientIP string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		username string
		storedIP sql.NullString
		tokenID  int
	)

	err := client.dbPool.QueryRow(ctx,
		`SELECT t.id, u.username, t.ip_address
		FROM auth_tokens t
		JOIN users u ON t.user_id = u.id
		WHERE t.token = $1 
		AND t.expires_at > NOW()`,
		token).Scan(&tokenID, &username, &storedIP)

	if err != nil {
		if err == pgx.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}

	if storedIP.Valid && storedIP.String != "" {
		if storedIP.String != clientIP {
			log.Printf("Warning: Token used from new IP. Original: %s, Current: %s",
				storedIP.String, clientIP)
		}
	}

	_, err = client.dbPool.Exec(ctx,
		`UPDATE auth_tokens 
		SET last_used_at = NOW(), 
		    ip_address = $1
		WHERE id = $2`,
		clientIP, tokenID)

	if err != nil {
		log.Printf("Error updating token usage: %v", err)
	}

	return username, true, nil
}

func genRandomUsername() string {
	return "user_" + strconv.Itoa(rand.Intn(1000))
}

func (client *Client) getClientIP() string {
	remoteAddr := client.conn.RemoteAddr().String()
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ip
}
