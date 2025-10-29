// === PORT MANAGER === \\
/* Queue, Matchmaking, and Port Allocation */
package portmanager

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

/* === Types === */
type PortManager struct {
	// PortManager manages game server port allocation and matchmaking queue
	mutex       sync.Mutex
	usedPorts   map[int]bool
	basePort    int
	maxPort     int
	currentPort int
	queue       []*QueuedPlayer
}
type QueuedPlayer struct {
	// QueuedPlayer represents a player in the matchmaking queue
	ID       string
	Username string
	Client   interface{} // *wsm.Client but we avoid direct import
}

/* === Global Vars === */
var (
	Manager *PortManager
	once        sync.Once
)

// NewPortManager creates a new port manager
func NewPortManager(basePort, maxPort int) *PortManager {
	return &PortManager{
		usedPorts:   make(map[int]bool),
		basePort:    basePort,
		maxPort:     maxPort,
		currentPort: basePort,
		queue:       make([]*QueuedPlayer, 0),
	}
}

/* === Class Methods === */
func (pm *PortManager) GetPort() (int, error) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	// Try to find an available port starting from currentPort
	startPort := pm.currentPort
	for {
		if !pm.usedPorts[pm.currentPort] {
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
func (pm *PortManager) ReleasePort(port int) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	delete(pm.usedPorts, port)
	log.Printf("Port %d released", port)
}
func (pm *PortManager) AddToQueue(id, username string, client interface{}) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	// Check if player is already in queue
	for _, p := range pm.queue {
		if p.ID == id {
			return // Already in queue
		}
	}

	player := &QueuedPlayer{
		ID:       id,
		Username: username,
		Client:   client,
	}
	pm.queue = append(pm.queue, player)
	log.Printf("Player %s added to queue. Queue size: %d", username, len(pm.queue))
}
func (pm *PortManager) RemoveFromQueue(id string) bool {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	for i, p := range pm.queue {
		if p.ID == id {
			// Remove player from queue
			pm.queue = append(pm.queue[:i], pm.queue[i+1:]...)
			log.Printf("Player %s removed from queue. Queue size: %d", p.Username, len(pm.queue))
			return true
		}
	}
	return false
}
func (pm *PortManager) GetQueueSize() int {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	return len(pm.queue)
}
func (pm *PortManager) StartQueueProcessor() {
	log.Println("Queue processor started")
	for {
		time.Sleep(1 * time.Second)
		
		pm.mutex.Lock()
		queueSize := len(pm.queue)
		if queueSize >= 2 {
			// Take first two players from queue
			players := pm.queue[:2]
			pm.queue = pm.queue[2:]
			pm.mutex.Unlock()

			// Start match in a separate goroutine
			go pm.startMatch(players)
		} else {
			pm.mutex.Unlock()
		}
	}
}
func (pm *PortManager) startMatch(players []*QueuedPlayer) {
	port, err := pm.GetPort()
	if err != nil {
		log.Printf("Failed to allocate port for match: %v", err)
		return
	}

	log.Printf("Starting match for players %s and %s on port %d", 
		players[0].Username, players[1].Username, port)

	// Generate a unique container name using player IDs
	containerName := fmt.Sprintf("game-server-%s-%s", 
		strings.Split(players[0].ID, ":")[0], 
		strings.Split(players[1].ID, ":")[0])

	// Start Podman container for match
	playersJSON, _ := json.Marshal([]string{players[0].Username, players[1].Username})

	cmd := exec.Command("podman", "run",
		"--name", containerName,
		"-d",
		"--rm",
		"-p", fmt.Sprintf("%d:7000", port),
		"-e", fmt.Sprintf("PLAYERS_JSON=%s", string(playersJSON)), // JSON
		"openchamp-game-server",
		"--port", "7000",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to start game container: %v\nOutput: %s", err, string(output))
		pm.ReleasePort(port)
		// Notify players of failure
		for _, player := range players {
			if client, ok := player.Client.(interface{ Send() chan []byte }); ok {
				errorInfo := map[string]interface{}{
					"type": "match_error",
					"payload": map[string]interface{}{
						"message": "Failed to start game server",
					},
				}
				errorJSON, _ := json.Marshal(errorInfo)
				client.Send() <- errorJSON
			}
		}
		return
	}

	containerID := strings.TrimSpace(string(output))
	log.Printf("Started game container: %s", containerID)

	// Give the container a moment to start up
	time.Sleep(2 * time.Second)

	// Notify players about the match
	for _, player := range players {
		if client, ok := player.Client.(interface{ Send() chan []byte }); ok {
			matchInfo := map[string]interface{}{
				"type": "match_found",
				"payload": map[string]interface{}{
					"port":        port,
					"players":     []string{players[0].Username, players[1].Username},
					"containerID": containerID,
				},
			}
			matchInfoJSON, _ := json.Marshal(matchInfo)
			client.Send() <- matchInfoJSON
		}
	}

	// Start monitoring the container
	go pm.monitorContainer(containerID, port)
}// Check if a port is available at the OS level
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
// InitPortManager initializes the global port manager instance (intentional singleton)
func InitPortManager(basePort, maxPort int) {
	once.Do(func() {
		Manager = NewPortManager(basePort, maxPort)
	})
}

/* === Package Port Management === */
func GetPort() (int, error) {
	if Manager == nil {
		return 0, fmt.Errorf("port manager not initialized")
	}
	return Manager.GetPort()
}

func ReleasePort(port int) {
	if Manager != nil {
		Manager.ReleasePort(port)
	}
}
// Watch a game container and clean up when it exits
func (pm *PortManager) monitorContainer(containerID string, port int) {
	// Wait for container exit
	cmd := exec.Command("podman", "wait", containerID)
	if err := cmd.Run(); err != nil {
		log.Printf("Error waiting for container %s: %v", containerID, err)
	}

	// Release port
	pm.ReleasePort(port)
	log.Printf("Container %s exited, port %d released", containerID, port)
}
