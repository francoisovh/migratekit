package progress

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"log"
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/k0kubun/go-ansi"
	"github.com/schollz/progressbar/v3"
	"github.com/vmware/govmomi/vim25/progress"

	"bufio"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var theme = progressbar.Theme{
	Saucer:        "[green]=[reset]",
	SaucerHead:    "[green]>[reset]",
	SaucerPadding: " ",
	BarStart:      "[",
	BarEnd:        "]",
}

func DataProgressBar(desc string, size int64) *progressbar.ProgressBar {
	return progressbar.NewOptions64(size,
		progressbar.OptionSetWriter(ansi.NewAnsiStdout()),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionShowCount(),
		progressbar.OptionUseIECUnits(true),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetDescription(desc),
		progressbar.OptionSetTheme(theme),
	)
}

func PercentageProgressBar(task string) *progressbar.ProgressBar {
	return progressbar.NewOptions64(100,
		progressbar.OptionSetWriter(ansi.NewAnsiStdout()),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowCount(),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetDescription(task),
		progressbar.OptionSetTheme(theme),
	)
}

type VMwareProgressBar struct {
	bar      *progressbar.ProgressBar
	ch       chan progress.Report
	reporter ProgressReporter
	jobID    string
}

type ProgressReporter interface {
	Percent(percent int, message string)
}

type ProgressMessage struct {
	Type    string `json:"type"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
	JobID   string `json:"job_id"`
}

type WebSocketProgressReporter struct {
	conn  *websocket.Conn
	jobID string
}

func NewWebSocketProgressReporter(serverURL string) (*WebSocketProgressReporter, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	jobID := u.Query().Get("job_id")
	dialer := websocket.Dialer{
		NetDial: (&net.Dialer{
			Timeout: 3 * time.Second,
		}).Dial,
	}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to WebSocket server: %w", err)
	}

	return &WebSocketProgressReporter{conn: conn, jobID: jobID}, nil
}

func (w *WebSocketProgressReporter) Percent(percent int, message string) {
	msg := ProgressMessage{
		Type:    "progress",
		Percent: percent,
		Message: message,
		JobID:   w.jobID,
	}

	if err := w.conn.WriteJSON(msg); err != nil {
		log.Printf("failed to send WebSocket progress message: %v", err)
	}
}
func (w *WebSocketProgressReporter) StreamLogs() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("error loading in-cluster config: %v", err)
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("error creating kubernetes client: %v", err)
		return
	}

	// Rebuild pod name from jobID
	podName := "migratekit-job-" + w.jobID[:8]
	namespace := "default"

	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: true,
	})

	stream, err := req.Stream(context.Background())
	if err != nil {
		log.Printf("error opening log stream for pod %s: %v", podName, err)
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		msg := ProgressMessage{
			Type:    "log",
			Message: line,
			JobID:   w.jobID,
		}

		if err := w.conn.WriteJSON(msg); err != nil {
			log.Printf("error writing log line to websocket: %v", err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("error reading from log stream: %v", err)
	}
}

func (w *WebSocketProgressReporter) Close() error {
	return w.conn.Close()
}

func NewVMwareProgressBar(jobID string, task string) *VMwareProgressBar {
	bar := PercentageProgressBar(task)

	reporter, err := NewWebSocketProgressReporter("ws://websocket-service.migratekit.svc.cluster.local/progress?job_id=" + jobID)
	if err != nil {
		log.Printf("failed to create websocket reporter, using none: %v", err)
		reporter = nil // fallback to just terminal bar
	}

	return &VMwareProgressBar{
		bar:      bar,
		ch:       make(chan progress.Report),
		reporter: reporter,
	}
}

func (p *VMwareProgressBar) Sink() chan<- progress.Report {
	return p.ch
}

func NewDataProgressReporter(desc string, size int64, reporter ProgressReporter, jobID string) *VMwareProgressBar {
	bar := DataProgressBar(desc, size)

	if reporter == nil {
		r, err := NewWebSocketProgressReporter("ws://websocket-service.migratekit.svc.cluster.local/progress?job_id=" + jobID)
		if err != nil {
			log.Printf("Failed to create WebSocket reporter: %v", err)
			reporter = nil
		} else {
			reporter = r

			// Start log streaming if reporter is WebSocket-based
			if wsReporter, ok := reporter.(*WebSocketProgressReporter); ok {
				go wsReporter.StreamLogs()
			}
		}
	}

	return &VMwareProgressBar{
		bar:      bar,
		ch:       make(chan progress.Report),
		reporter: reporter,
	}
}

func (v *VMwareProgressBar) Bar() *progressbar.ProgressBar {
	return v.bar
}

func (v *VMwareProgressBar) Reporter() ProgressReporter {
	return v.reporter
}

func (u *VMwareProgressBar) Loop(done <-chan struct{}) {
	defer func() {
		// Clean WebSocket connection if it's used
		if ws, ok := u.reporter.(*WebSocketProgressReporter); ok {
			ws.Close()
		}
	}()

	for {
		select {
		case <-done:
			return
		case report, ok := <-u.ch:
			if !ok {
				return
			}
			if err := report.Error(); err != nil {
				return
			}

			pct := int(report.Percentage())
			u.bar.Set(pct)

			detail := report.Detail()
			if detail != "" {
				u.bar.Describe(detail)
			}

			if u.reporter != nil {
				u.reporter.Percent(pct, detail)
			}
		}
	}
}
