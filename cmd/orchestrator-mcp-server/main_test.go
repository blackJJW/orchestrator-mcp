package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func TestRunHTTPMCP(t *testing.T) {
	profilePath, err := filepath.Abs("../../testdata/projects/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		err := run(ctx, []string{"-profile", profilePath, "-port", "0"}, writer)
		writer.Close()
		done <- err
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	endpoint := strings.TrimSpace(strings.TrimPrefix(line, "MCP endpoint: "))
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/mcp" {
		t.Fatalf("non-loopback endpoint: %s %v", endpoint, err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "entrypoint-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "check_scope" {
		t.Fatalf("discovery: %+v %v", tools, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "check_scope", Arguments: core.Request{
		TaskID: "bootstrap-check", Workspace: profile.WorkspaceRoot, Permission: "source.read",
	}})
	if err != nil || result.IsError {
		t.Fatalf("call: %+v %v", result, err)
	}
	data, _ := json.Marshal(result.StructuredContent)
	var decision core.Decision
	if err := json.Unmarshal(data, &decision); err != nil || !decision.ScopeAllowed || decision.ExecutionStarted || decision.Reason != "DRY_RUN_ONLY" {
		t.Fatalf("decision: %s %v", data, err)
	}
	t.Logf("actual entrypoint %s initialize/tools/list/tools/call: %s", endpoint, data)
	// Probe only our just-created port on an address owned by this host.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.To4() == nil || ip.IsLoopback() {
			continue
		}
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort(ip.String(), u.Port()), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("loopback server accepted non-loopback connection")
		}
		t.Log("non-loopback connection to own server port rejected")
		break
	}
	session.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestStartupRejects(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-profile", "missing.json"},
		{"-profile", "missing.json", "-port", "-1"},
		{"-profile", "missing.json", "-port", "65536"},
		{"-profile", "missing.json", "unexpected"},
		{"-profile", "missing.json", "-listen", "0.0.0.0:8080"},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("startup accepted %q", args)
		}
	}
	if err := run(context.Background(), []string{"-help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSourceSpecStopsBeforeListen(t *testing.T) {
	profile, err := filepath.Abs("../../testdata/projects/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(t.TempDir(), "invalid-source.json")
	if err := os.WriteFile(spec, []byte(`{"contract":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := run(context.Background(), []string{"-profile", profile, "-source-spec", spec, "-port", "0"}, &output); err == nil || output.Len() != 0 {
		t.Fatalf("invalid source binding opened a listener: %v %s", err, output.String())
	}
}

func TestContainerListenRejectedOnHost(t *testing.T) {
	if _, err := os.Lstat("/.dockerenv"); err == nil {
		t.Skip("running inside Docker; container listener is covered by Compose integration")
	}
	profile, err := filepath.Abs("../../testdata/projects/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := run(context.Background(), []string{"-profile", profile, "-container-listen", "-port", "0"}, &output); err == nil || output.Len() != 0 {
		t.Fatalf("container-only binding accepted on host: %v %s", err, output.String())
	}
}

func TestInvalidRoutingPolicyStopsBeforeListen(t *testing.T) {
	var output strings.Builder
	err := run(context.Background(), []string{"-profile", "../../testdata/projects/alpha.json", "-routing-policy", "../../testdata/routing/beta.json", "-port", "0"}, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("invalid routing policy opened a listener: %v %s", err, output.String())
	}
}
