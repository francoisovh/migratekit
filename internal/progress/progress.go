package progress

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"log"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/k0kubun/go-ansi"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
	"github.com/vmware/govmomi/vim25/progress"
)

type WebSocketLogHook struct {
	conn  *websocket.Conn
	jobID string
}

type MigrationJob struct {
	JobID     string `json:"job_id"`
	VMName    string `json:"vm_name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func NewWebSocketLogHook(serverURL, jobID string) (*WebSocketLogHook, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	q := u.Query()
	q.Set("job_id", jobID)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dial error: %w", err)
	}

	return &WebSocketLogHook{conn: conn, jobID: jobID}, nil
}

func (h *WebSocketLogHook) Fire(entry *logrus.Entry) error {
	msg, err := entry.String()
	if err != nil {
		return err
	}

	data, _ := json.Marshal(ProgressMessage{
		Type:    "log",
		Message: msg,
		JobID:   h.jobID,
	})
	return h.conn.WriteMessage(websocket.TextMessage, data)
}

func (h *WebSocketLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

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
}

type ProgressReporter interface {
	Percent(percent int, message string)
}

type ProgressMessage struct {
	Type    string `json:"type"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
	JobID   string `json:"job_id"`
	VMName  string `json:"vm_name"`
}

type WebSocketProgressReporter struct {
	conn   *websocket.Conn
	jobID  string
	VMName string
}

func getVMNameFromAPI(jobID string) string {
	url := fmt.Sprintf("http://migratekit-svc.migratekit.svc.cluster.local/api/migration-jobs/%s/", jobID)

	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Error fetching job info: %v", err)
		return "unknown-vm"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "unknown-vm"
	}

	var job MigrationJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		log.Printf("Error decoding job info: %v", err)
		return "unknown-vm"
	}

	return job.VMName
}

func NewWebSocketProgressReporter(serverURL string) (*WebSocketProgressReporter, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	jobID := u.Query().Get("job_id")
	vmName := getVMNameFromAPI(jobID)
	dialer := websocket.Dialer{
		NetDial: (&net.Dialer{
			Timeout: 3 * time.Second,
		}).Dial,
	}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to WebSocket server: %w", err)
	}

	return &WebSocketProgressReporter{conn: conn, jobID: jobID, VMName: vmName}, nil
}

func (w *WebSocketProgressReporter) Percent(percent int, message string) {

	msg := ProgressMessage{
		Type:    "progress",
		Percent: percent,
		Message: message,
		JobID:   w.jobID,
		VMName:  w.VMName,
	}

	if err := w.conn.WriteJSON(msg); err != nil {
		log.Printf("failed to send WebSocket progress message: %v", err)
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

	// Connect WebSocket log hook
	hook, err := NewWebSocketLogHook("ws://websocket-service.migratekit.svc.cluster.local/progress?job_id=", jobID)
	if err != nil {
		logrus.Warnf("websocket hook disabled: %v", err)
	} else {
		logrus.AddHook(hook)
	}

	logrus.Info(task)

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
		}
	}

	// Connect WebSocket log hook
	hook, err := NewWebSocketLogHook("ws://websocket-service.migratekit.svc.cluster.local/progress?job_id=", jobID)
	if err != nil {
		logrus.Warnf("websocket hook disabled: %v", err)
	} else {
		logrus.AddHook(hook)
	}

	logrus.Info(desc)

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
