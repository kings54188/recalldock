package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const FrameLimit = 8 << 20
const CatalogLimit = 2 << 20

var ErrFrameLimit = errors.New("backend response frame exceeds the configured size limit")

type frameReader struct {
	reader  *bufio.Reader
	limit   int
	pending []byte
}

func newFrameReader(reader io.Reader, limit int) *frameReader {
	return &frameReader{reader: bufio.NewReader(reader), limit: limit}
}

func (r *frameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		for {
			part, err := r.reader.ReadSlice('\n')
			if len(r.pending)+len(part) > r.limit {
				r.pending = nil
				return 0, ErrFrameLimit
			}
			r.pending = append(r.pending, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				r.pending = nil
				return 0, err
			}
			break
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

type connection struct {
	client        *client.Client
	cancel        context.CancelFunc
	cmd           *exec.Cmd
	stdout        io.ReadCloser
	done          chan struct{}
	closed        atomic.Bool
	once          sync.Once
	closeErr      error
	httpTransport *http.Transport
	progressMu    sync.Mutex
	progress      map[string]func(mcp.JSONRPCNotification)
}

type quietLogger struct{}

func (quietLogger) Infof(string, ...any)  {}
func (quietLogger) Errorf(string, ...any) {}

func connect(ctx context.Context, runtime Runtime, root string) (*connection, error) {
	life, cancel := context.WithCancel(context.Background())
	c := &connection{cancel: cancel, progress: map[string]func(mcp.JSONRPCNotification){}}
	var wire transport.Interface
	switch runtime.Type {
	case "stdio":
		env, err := runtime.Environment()
		if err != nil {
			cancel()
			return nil, err
		}
		command := runtime.Command
		if !filepath.IsAbs(command) {
			command, err = backendCommand(command, env)
			if err != nil {
				cancel()
				return nil, ErrUnavailable
			}
		}
		cmd := exec.Command(command, runtime.Args...)
		cmd.Dir = runtime.CWD
		if cmd.Dir == "" {
			cmd.Dir = root
		}
		if !filepath.IsAbs(cmd.Dir) {
			cancel()
			return nil, errors.New("backend cwd must be absolute")
		}
		cmd.Env = env
		if err := configureProcess(cmd); err != nil {
			cancel()
			return nil, err
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return nil, ErrUnavailable
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			cancel()
			return nil, ErrUnavailable
		}
		// A nil Stderr is the null device, not a buffering pipe into our context.
		if err := cmd.Start(); err != nil {
			stdin.Close()
			stdout.Close()
			cancel()
			return nil, ErrUnavailable
		}
		c.cmd, c.stdout, c.done = cmd, stdout, make(chan struct{})
		go func() { _ = cmd.Wait(); close(c.done) }()
		wire = transport.NewIO(newFrameReader(stdout, FrameLimit), stdin, nil)
	case "http", "sse":
		endpoint, err := url.Parse(runtime.URL)
		if err != nil || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
			cancel()
			return nil, ErrUnavailable
		}
		if endpoint.Scheme == "http" {
			ip := net.ParseIP(endpoint.Hostname())
			if endpoint.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
				cancel()
				return nil, errors.New("cleartext remote MCP is not supported")
			}
		}
		base := http.DefaultTransport.(*http.Transport).Clone()
		c.httpTransport = base
		httpClient := &http.Client{Transport: &boundedHTTP{base: base, headers: runtime.Headers, origin: endpoint.Scheme + "://" + endpoint.Host}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("MCP redirects require explicit operator configuration")
		}}
		if runtime.Type == "http" {
			wire, err = transport.NewStreamableHTTP(runtime.URL, transport.WithHTTPBasicClient(httpClient), transport.WithHTTPLogger(quietLogger{}))
		} else {
			wire, err = transport.NewSSE(runtime.URL, transport.WithHTTPClient(httpClient))
		}
		if err != nil {
			cancel()
			return nil, ErrUnavailable
		}
	default:
		cancel()
		return nil, errors.New("unsupported MCP transport")
	}
	c.client = client.NewClient(cancellingTransport{wire})
	c.client.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != "notifications/progress" {
			return
		}
		token, ok := n.Params.AdditionalFields["progressToken"].(string)
		if !ok {
			return
		}
		c.progressMu.Lock()
		fn := c.progress[token]
		c.progressMu.Unlock()
		if fn != nil {
			fn(n)
		}
	})
	if err := c.client.Start(life); err != nil {
		c.Close()
		return nil, ErrUnavailable
	}
	_, err := c.client.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "recalldock-gateway", Version: "1"}}})
	if err != nil {
		c.Close()
		return nil, ErrUnavailable
	}
	return c, nil
}

// Resolve against the environment the child will actually receive. LookPath
// uses the gateway's process PATH, which can differ from a declared backend
// PATH in GUI hosts. Never search implicit or relative current directories.
func backendCommand(name string, env []string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\\`) {
		return "", ErrUnavailable
	}
	var rawPath, rawPathExt string
	for _, entry := range env {
		if runtime.GOOS == "windows" {
			if len(entry) >= 5 && strings.EqualFold(entry[:5], "PATH=") {
				rawPath = entry[5:]
			} else if len(entry) >= 8 && strings.EqualFold(entry[:8], "PATHEXT=") {
				rawPathExt = entry[8:]
			}
		} else if strings.HasPrefix(entry, "PATH=") {
			rawPath = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	if rawPath == "" {
		return "", ErrUnavailable
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		if filepath.Ext(name) != "" {
			exts = []string{""}
		} else {
			if rawPathExt == "" {
				rawPathExt = ".exe;.cmd;.bat;.com"
			}
			exts = nil
			seen := map[string]bool{}
			for _, ext := range strings.Split(rawPathExt, ";") {
				ext = strings.TrimSpace(ext)
				if ext == "" {
					continue
				}
				if !strings.HasPrefix(ext, ".") {
					ext = "." + ext
				}
				lower := strings.ToLower(ext)
				if !seen[lower] {
					seen[lower] = true
					exts = append(exts, lower)
				}
			}
		}
	}
	for _, dir := range filepath.SplitList(rawPath) {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, ext := range exts {
			path := filepath.Join(dir, name+ext)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0) {
				return path, nil
			}
		}
	}
	return "", ErrUnavailable
}

func (c *connection) Alive() bool {
	if c.closed.Load() {
		return false
	}
	if c.done != nil {
		select {
		case <-c.done:
			return false
		default:
		}
	}
	return true
}

func (c *connection) Close() error {
	c.once.Do(func() {
		c.closed.Store(true)
		c.cancel()
		if c.client != nil {
			_ = c.client.Close()
		}
		if c.cmd != nil {
			select {
			case <-c.done:
			default:
				terminateProcess(c.cmd)
				select {
				case <-c.done:
				case <-time.After(400 * time.Millisecond):
					killProcess(c.cmd)
					select {
					case <-c.done:
					case <-time.After(time.Second):
						c.closeErr = errors.New("owned backend did not exit within shutdown deadline")
					}
				}
			}
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.httpTransport != nil {
			c.httpTransport.CloseIdleConnections()
		}
	})
	return c.closeErr
}

func (c *connection) Tools(ctx context.Context) ([]mcp.Tool, error) {
	var request mcp.ListToolsRequest
	result := []mcp.Tool{}
	cursors := map[mcp.Cursor]bool{}
	names := map[string]bool{}
	total := 0
	for page := 0; page < 20; page++ {
		listed, err := c.client.ListToolsByPage(ctx, request)
		if err != nil {
			return nil, ErrUnavailable
		}
		for _, tool := range listed.Tools {
			body, err := json.Marshal(tool)
			if err != nil || len(body) > 128<<10 || tool.Name == "" || len(tool.Name) > 200 || names[tool.Name] {
				return nil, errors.New("invalid or duplicate tool definition")
			}
			total += len(body)
			if total > CatalogLimit || len(result) >= 1000 {
				return nil, errors.New("backend tool catalog exceeds bounded discovery limits")
			}
			names[tool.Name] = true
			result = append(result, tool)
		}
		if listed.NextCursor == "" {
			return result, nil
		}
		if len(listed.NextCursor) > 1000 || cursors[listed.NextCursor] {
			return nil, errors.New("backend repeated an invalid pagination cursor")
		}
		cursors[listed.NextCursor] = true
		request.Params.Cursor = listed.NextCursor
	}
	return nil, errors.New("backend tool catalog has too many pages")
}

func (c *connection) Call(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return c.client.CallTool(ctx, request)
}

func (c *connection) CallWithProgress(ctx context.Context, request mcp.CallToolRequest, notify func(mcp.JSONRPCNotification)) (*mcp.CallToolResult, error) {
	if request.Params.Meta == nil || request.Params.Meta.ProgressToken == nil || notify == nil {
		return c.Call(ctx, request)
	}
	original := request.Params.Meta.ProgressToken
	token := randomToken()
	// Each live call owns a different token even if concurrent host calls reuse theirs.
	c.progressMu.Lock()
	c.progress[token] = func(n mcp.JSONRPCNotification) {
		fields := map[string]any{}
		for k, v := range n.Params.AdditionalFields {
			fields[k] = v
		}
		fields["progressToken"] = original
		n.Params.AdditionalFields = fields
		notify(n)
	}
	c.progressMu.Unlock()
	defer func() { c.progressMu.Lock(); delete(c.progress, token); c.progressMu.Unlock() }()
	request.Params.Meta = &mcp.Meta{ProgressToken: token}
	return c.Call(ctx, request)
}

func (c *connection) Notify(handler func(mcp.JSONRPCNotification)) { c.client.OnNotification(handler) }

// Context cancellation alone removes the client's waiter. Also forward the
// MCP cancellation notification; it is best effort, never proof of rollback.
type cancellingTransport struct{ transport.Interface }

func (t cancellingTransport) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	response, err := t.Interface.SendRequest(ctx, request)
	if err != nil && ctx.Err() != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = t.Interface.SendNotification(cancelCtx, mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{Method: "notifications/cancelled", Params: mcp.NotificationParams{AdditionalFields: map[string]any{"requestId": request.ID, "reason": "upstream request cancelled"}}}})
	}
	return response, err
}

type boundedHTTP struct {
	base    http.RoundTripper
	headers map[string]string
	origin  string
}

func (t *boundedHTTP) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme+"://"+request.URL.Host != t.origin {
		return nil, errors.New("cross-origin MCP request refused")
	}
	request = request.Clone(request.Context())
	for key, value := range t.headers {
		request.Header.Set(key, value)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body = &boundedBody{Reader: newFrameReader(response.Body, FrameLimit), Closer: response.Body}
	} else {
		response.Body = &boundedBody{Reader: &limitedBody{reader: response.Body, left: FrameLimit}, Closer: response.Body}
	}
	return response, nil
}

type boundedBody struct {
	io.Reader
	io.Closer
}
type limitedBody struct {
	reader io.Reader
	left   int
}

func (r *limitedBody) Read(p []byte) (int, error) {
	if r.left <= 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n > 0 {
			return 0, ErrFrameLimit
		}
		return n, err
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	r.left -= n
	return n, err
}
