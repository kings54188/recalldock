package candidatehook

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proposal delivery is local IPC over HTTP, not cloud replication. Literal
// loopback addresses avoid DNS/localhost rebinding. No credentials or URL options.
func validateLocalEndpoint(endpoint string) error {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("candidate endpoint must be a literal loopback HTTP URL")
	}
	ip := net.ParseIP(u.Hostname())
	if (u.Scheme != "http" && u.Scheme != "https") || ip == nil || !ip.IsLoopback() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("candidate endpoint must be a literal loopback HTTP URL without credentials, path, query or fragment")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return fmt.Errorf("candidate endpoint has invalid port")
		}
	}
	return nil
}

func localHTTPClient(configured *http.Client, timeout time.Duration) *http.Client {
	var client http.Client
	if configured != nil {
		client = *configured
	}
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client.Transport = transport
	} else if transport, ok := client.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.Proxy = nil
		client.Transport = transport
	}
	return &client
}
