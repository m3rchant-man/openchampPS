package webapi

import (
	"encoding/json"
	"log"
	"net/http"
	"openchamp/server/wsm"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewServer(manager *wsm.ClientManager, dbPool *pgxpool.Pool, listenAddr string) *http.Server {
	router := http.NewServeMux()
	// WebSocket endpoint
	router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		manager.HandleWebSocket(w, r, dbPool)
	})

	// Health check endpoint
	router.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// User registration endpoint
	router.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
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
	server := &http.Server{
		Addr:    listenAddr,
		Handler: router,
	}

	return server
}
// StartServer starts the provided http.Server
func StartServer(server *http.Server) {
	log.Printf("Server starting on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("Server stopped: %v", err)
	}
}
