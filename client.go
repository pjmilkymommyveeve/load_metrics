package main

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
)

var wsServer string
var interval time.Duration

func init() {
	wsServer = "wss://loadmetrics.xdialnetworks.com/ws/agent"
	interval = 5 * time.Second
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

type MetricsPayload struct {
	Hostname  string        `json:"hostname"`
	IP        string        `json:"ip"`
	Timestamp int64         `json:"timestamp"`
	CPU       CPUMetrics    `json:"cpu"`
	Disk      []DiskMetrics `json:"disk"`
	Load      LoadMetrics   `json:"load"`
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "unknown"
}

func collectMetrics() (*MetricsPayload, error) {
	hostInfo, _ := host.Info()
	cpuPerCore, _ := cpu.Percent(0, true)
	cpuTotal, _ := cpu.Percent(0, false)
	partitions, _ := disk.Partitions(false)
	loadAvg, _ := load.Avg()

	var disks []DiskMetrics
	for _, p := range partitions {
		if p.Mountpoint == "/" {
			usage, err := disk.Usage(p.Mountpoint)
			if err == nil {
				disks = append(disks, DiskMetrics{
					Mount:       p.Mountpoint,
					Total:       usage.Total,
					Used:        usage.Used,
					UsedPercent: usage.UsedPercent,
				})
			}
		}
	}

	return &MetricsPayload{
		Hostname:  hostInfo.Hostname,
		IP:        getLocalIP(),
		Timestamp: time.Now().Unix(),
		CPU: CPUMetrics{
			PerCore:      cpuPerCore,
			TotalPercent: cpuTotal[0],
		},
		Disk: disks,
		Load: LoadMetrics{
			Load1:  loadAvg.Load1,
			Load5:  loadAvg.Load5,
			Load15: loadAvg.Load15,
		},
	}, nil
}

func main() {
	log.Println("Starting metrics agent...")
	hostname, _ := os.Hostname()

	for {
		conn, _, err := websocket.DefaultDialer.Dial(wsServer, nil)
		if err != nil {
			log.Println("WebSocket connection failed, retrying in 5s:", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("Connected to server as", hostname)

		conn.SetPingHandler(func(appData string) error {
			conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
			return nil
		})

		ticker := time.NewTicker(interval)
		done := make(chan struct{})

		go func() {
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					close(done)
					return
				}
			}
		}()

		func() {
			for {
				select {
				case <-ticker.C:
					metrics, err := collectMetrics()
					if err != nil {
						log.Println("Metric collection error:", err)
						continue
					}

					log.Printf("Load Avg → 1min: %.2f, 5min: %.2f, 15min: %.2f",
						metrics.Load.Load1, metrics.Load.Load5, metrics.Load.Load15)

					data, _ := json.Marshal(metrics)
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					err = conn.WriteMessage(websocket.TextMessage, data)
					if err != nil {
						log.Println("WebSocket send error, reconnecting:", err)
						return
					}
				case <-done:
					return
				}
			}
		}()

		ticker.Stop()
		conn.Close()
		log.Println("Connection lost, reconnecting...")
		time.Sleep(2 * time.Second)
	}
}
