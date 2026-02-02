package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type MetricsPayload struct {
	Hostname  string        `json:"hostname"`
	IP        string        `json:"ip"`
	Timestamp int64         `json:"timestamp"`
	CPU       CPUMetrics    `json:"cpu"`
	Disk      []DiskMetrics `json:"disk"`
	Load      LoadMetrics   `json:"load"`
}

type CPUMetrics struct {
	PerCore      []float64 `json:"per_core"`
	TotalPercent float64   `json:"total_percent"`
}

type DiskMetrics struct {
	Mount       string  `json:"mount"`
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	UsedPercent float64 `json:"used_percent"`
}

type LoadMetrics struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type Threshold struct {
	CPU  int `json:"cpu"`
	Disk int `json:"disk"`
	Load int `json:"load"`
}

type ThresholdConfig struct {
	Thresholds map[string]Threshold `json:"thresholds"`
	mu         sync.RWMutex
}

type Hub struct {
	agents    map[string]*Agent
	dashboard map[*websocket.Conn]bool
	mu        sync.RWMutex
	broadcast chan *MetricsPayload
	config    *ThresholdConfig
}

type Agent struct {
	conn        *websocket.Conn
	lastMetrics *MetricsPayload
	lastSeen    time.Time
}

func NewThresholdConfig(filename string) *ThresholdConfig {
	tc := &ThresholdConfig{
		Thresholds: make(map[string]Threshold),
	}

	if data, err := ioutil.ReadFile(filename); err == nil {
		if err := json.Unmarshal(data, tc); err != nil {
			log.Println("Error parsing threshold config:", err)
		} else {
			log.Println("Loaded threshold config from", filename)
		}
	} else {
		log.Println("No existing threshold config found, will create new one")
	}

	return tc
}

func (tc *ThresholdConfig) Save(filename string) error {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	data, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filename, data, 0644)
}

func (tc *ThresholdConfig) Get(hostname string) (Threshold, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	t, ok := tc.Thresholds[hostname]
	return t, ok
}

func (tc *ThresholdConfig) Set(hostname string, threshold Threshold) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.Thresholds[hostname] = threshold
}

func NewHub(configFile string) *Hub {
	h := &Hub{
		agents:    make(map[string]*Agent),
		dashboard: make(map[*websocket.Conn]bool),
		broadcast: make(chan *MetricsPayload, 256),
		config:    NewThresholdConfig(configFile),
	}
	go h.run()
	return h
}

func (h *Hub) run() {
	for metrics := range h.broadcast {
		h.mu.RLock()
		data, _ := json.Marshal(metrics)
		for conn := range h.dashboard {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Println("Dashboard write error:", err)
				conn.Close()
				delete(h.dashboard, conn)
			}
		}
		h.mu.RUnlock()
	}
}

func (h *Hub) agentHandler(c echo.Context) error {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	var agentID string

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if agentID != "" {
				h.mu.Lock()
				delete(h.agents, agentID)
				h.mu.Unlock()
				log.Println("Agent disconnected:", agentID)
			}
			break
		}

		var metrics MetricsPayload
		if err := json.Unmarshal(msg, &metrics); err != nil {
			log.Println("Invalid metrics format:", err)
			continue
		}

		if agentID == "" {
			agentID = metrics.IP
			h.mu.Lock()
			h.agents[agentID] = &Agent{
				conn:     ws,
				lastSeen: time.Now(),
			}
			h.mu.Unlock()
			log.Printf("Agent connected: %s (IP: %s)", metrics.Hostname, agentID)
		}

		h.mu.Lock()
		if agent, ok := h.agents[agentID]; ok {
			agent.lastMetrics = &metrics
			agent.lastSeen = time.Now()
		}
		h.mu.Unlock()

		select {
		case h.broadcast <- &metrics:
		default:
			log.Println("Broadcast channel full, dropping metrics")
		}
	}

	return nil
}

func (h *Hub) dashboardHandler(c echo.Context) error {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	h.mu.Lock()
	h.dashboard[ws] = true
	h.mu.Unlock()

	log.Println("Dashboard connected:", ws.RemoteAddr())

	h.mu.RLock()
	for _, agent := range h.agents {
		if agent.lastMetrics != nil {
			data, _ := json.Marshal(agent.lastMetrics)
			ws.WriteMessage(websocket.TextMessage, data)
		}
	}
	h.mu.RUnlock()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				h.mu.Lock()
				delete(h.dashboard, ws)
				h.mu.Unlock()
				log.Println("Dashboard disconnected:", ws.RemoteAddr())
				return nil
			}
		}
	}
}

func (h *Hub) statusHandler(c echo.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status := make(map[string]interface{})
	agents := make([]map[string]interface{}, 0, len(h.agents))

	for ip, agent := range h.agents {
		agentInfo := map[string]interface{}{
			"ip":        ip,
			"last_seen": agent.lastSeen.Unix(),
		}
		if agent.lastMetrics != nil {
			agentInfo["hostname"] = agent.lastMetrics.Hostname
		}
		agents = append(agents, agentInfo)
	}

	status["agents"] = agents
	status["agent_count"] = len(h.agents)
	status["dashboard_count"] = len(h.dashboard)

	return c.JSON(http.StatusOK, status)
}

func (h *Hub) getThresholdsHandler(c echo.Context) error {
	h.config.mu.RLock()
	defer h.config.mu.RUnlock()

	return c.JSON(http.StatusOK, h.config.Thresholds)
}

func (h *Hub) setThresholdHandler(c echo.Context) error {
	var req struct {
		Hostname  string    `json:"hostname"`
		Threshold Threshold `json:"threshold"`
	}

	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	h.config.Set(req.Hostname, req.Threshold)

	configFile := os.Getenv("THRESHOLD_CONFIG")
	if configFile == "" {
		configFile = "thresholds.json"
	}

	if err := h.config.Save(configFile); err != nil {
		log.Println("Error saving threshold config:", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to save config"})
	}

	log.Printf("Saved threshold for %s: CPU=%d%%, Disk=%d%%, Load=%d\n",
		req.Hostname, req.Threshold.CPU, req.Threshold.Disk, req.Threshold.Load)

	return c.JSON(http.StatusOK, map[string]string{"status": "success"})
}

func main() {
	configFile := os.Getenv("THRESHOLD_CONFIG")
	if configFile == "" {
		configFile = "thresholds.json"
	}

	hub := NewHub(configFile)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	e.GET("/ws/agent", hub.agentHandler)
	e.GET("/ws/dashboard", hub.dashboardHandler)
	e.GET("/api/status", hub.statusHandler)
	e.GET("/api/thresholds", hub.getThresholdsHandler)
	e.POST("/api/thresholds", hub.setThresholdHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}

	log.Println("Metrics server listening on :" + port)
	log.Println("Threshold config file:", configFile)
	if err := e.Start(":" + port); err != nil {
		log.Fatal("Server failed:", err)
	}
}
