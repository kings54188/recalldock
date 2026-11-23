package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/operatorui"
	"recalldock.local/recalldock/internal/store"
)

// cmdUI starts the local operator web surface. It is a separate process and a
// separate trust boundary from `serve`/MCP: it prints a one-time bearer token
// on the terminal, binds loopback only, and never exposes model transports.
func cmdUI(cfg store.Config) {
	port := 7438
	project := ""
	open := false
	readOnly := false
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--port" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 65535 {
				fatal(fmt.Errorf("--port must be 1..65535"))
				return
			}
			port = n
			i++
		case strings.HasPrefix(args[i], "--port="):
			n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--port="))
			if err != nil || n < 1 || n > 65535 {
				fatal(fmt.Errorf("--port must be 1..65535"))
				return
			}
			port = n
		case args[i] == "--project" && i+1 < len(args):
			project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		case args[i] == "--open":
			open = true
		case args[i] == "--read-only":
			readOnly = true
		case args[i] == "--help" || args[i] == "-h":
			fmt.Println("usage: recalldock ui [--port 7438] [--project PROJECT] [--open] [--read-only]")
			fmt.Println("Starts the loopback operator UI; the printed URL contains a one-time token.")
			return
		default:
			fatal(fmt.Errorf("unknown ui option %q", args[i]))
			return
		}
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ui, err := operatorui.New(operatorui.Options{Store: s, Version: uiDisplayVersion(version), Project: project, ReadOnly: readOnly})
	if err != nil {
		fatal(err)
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := ui.Listen(addr)
	if err != nil {
		fatal(err)
		return
	}
	url := ui.URL(addr)
	fmt.Println("RecallDock 操作界面：")
	fmt.Println(url)
	fmt.Println("这个 URL 含一次性 token；只在运行 `recalldock ui` 的终端里使用，不要贴到聊天或日志里。")
	if readOnly {
		fmt.Println("只读模式：不提供归档、候选批准等写操作。")
	}
	if open {
		if runtime.GOOS == "windows" {
			_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		} else {
			_ = exec.Command("xdg-open", url).Start()
		}
	}
	if err := ui.Serve(ln); err != nil {
		fatal(err)
	}
}

// uiDisplayVersion trims Go module pseudo-versions (dev builds) to "dev" so the
// header does not show a 40-character build string with a dirty marker.
var pseudoVersion = regexp.MustCompile(`-\d{14}-[0-9a-f]{12}`)

func uiDisplayVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "dev"
	}
	if strings.HasSuffix(value, "+dirty") || pseudoVersion.MatchString(value) {
		return "dev"
	}
	return value
}
