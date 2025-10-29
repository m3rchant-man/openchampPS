package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	
	"openchamp/server/config"
	"openchamp/server/portmanager"
	"openchamp/server/webapi"
	"openchamp/server/wsm"
)

func main() {
	log.Println("Starting OpenChamp server...")
	
	/* === Database Config === */
	DBPool, err := config.InitDBPool()
	if err != nil {
		panic("Failed to initialize database pool: " + err.Error())
	}
	if err := config.SetupDatabase(DBPool); err != nil {
		log.Printf("Warning: Database setup issue: %v", err)
	}
	defer DBPool.Close()

	/* === WebSocket and Web Server Setup === */
	portmanager.InitPortManager(config.BaseGamePort, config.MaxGamePort)
	
	WSClientManager := wsm.NewClientManager()
	WebServer := webapi.NewServer(WSClientManager, DBPool, config.ListenAddr)

	// Start the HTTP server with graceful shutdown
	go webapi.StartServer(WebServer)
	go WSClientManager.Start()
	go portmanager.Manager.StartQueueProcessor()

	log.Printf("Server is running on %s", config.ListenAddr)
	log.Printf("Game server ports range: %d-%d", config.BaseGamePort, config.MaxGamePort)

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-sigChan
	log.Printf("Received signal %v, initiating graceful shutdown...", sig)

	// Create a timeout context for shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Initiate graceful shutdown
	if err := WebServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error during server shutdown: %v", err)
	}

	// Close all active WebSocket connections
	WSClientManager.Shutdown()

	log.Println("Server shutdown complete")
}
