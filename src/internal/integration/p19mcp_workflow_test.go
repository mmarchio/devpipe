package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// p19mcpSecret is the MCP bearer token of the tests.
const p19mcpSecret = "p19-mcp-secret"

// p19mcpConfig enables the MCP server on a free port.
func p19mcpConfig(t *testing.T) func(*config.Config) {
	t.Setenv("P19_MCP_KEY", p19mcpSecret)
	return func(c *config.Config) {
		c.MCP = config.MCPConfig{Enabled: true, Addr: "127.0.0.1:0", AuthSecretEnv: "P19_MCP_KEY"}
	}
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// mcpClient is an MCP client session against the overseer, like Hermes.
type mcpClient struct {
	t  *testing.T
	cs *sdk.ClientSession
}

func p19mcpConnect(t *testing.T, addr, secret string) (*mcpClient, error) {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "hermes-test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{secret, http.DefaultTransport}}}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &mcpClient{t: t, cs: cs}, nil
}

// call returns the tool's text and whether it is a tool error; a protocol
// error is returned as err.
func (c *mcpClient) call(name string, args map[string]any) (string, bool, error) {
	res, err := c.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError, nil
}

// must calls a tool that has to succeed and decodes its JSON result.
func (c *mcpClient) must(name string, args map[string]any) map[string]any {
	c.t.Helper()
	text, isErr, err := c.call(name, args)
	if err != nil || isErr {
		c.t.Errorf("%s %v: %s %v", name, args, text, err)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		c.t.Errorf("%s: %q is not JSON: %v", name, text, err)
	}
	return m
}

var tokenRE = regexp.MustCompile(`ovt_[0-9a-f]{64}`)

// mcpTeam simulates the Hermes team: it answers each command through the
// overseer MCP tools (never statefiles).
type mcpTeam struct {
	t      *testing.T
	r      *p16Rig
	mcp    *mcpClient
	mu     sync.Mutex
	cmds   []string
	tested map[string]int
	keys   int
	// failFirstTest leaves the first test of this task with a check unmet.
	failFirstTest string
	// silentTests answers this many test commands without a result.
	silentTests int
	// tokens are the tokens received, in order.
	tokens []string
	// during runs once, while the command with this first line is in
	// flight, before the team answers it; true means it answered the
	// command itself.
	during map[string]func(tok string) bool
}

func (s *mcpTeam) key() string {
	s.keys++
	return fmt.Sprintf("k%d", s.keys)
}

func (s *mcpTeam) commandLines() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.cmds))
	for _, c := range s.cmds {
		first, _, _ := strings.Cut(c, "\n")
		if strings.HasPrefix(first, "create the ") && strings.HasSuffix(first, "-output-format skill") {
			first = "create the output-format skill"
		}
		out = append(out, first)
	}
	return strings.Join(out, " | ")
}

func (s *mcpTeam) respond(m hermestest.Message) *hermestest.Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, m.Content)
	tok := tokenRE.FindString(m.Content)
	if tok == "" {
		s.t.Errorf("command without a token: %q", m.Content)
	}
	s.tokens = append(s.tokens, tok)
	cmd, _, _ := strings.Cut(m.Content, "\n")
	if f := s.during[cmd]; f != nil {
		delete(s.during, cmd)
		if f(tok) {
			reply := "done: " + cmd
			return &hermestest.Behavior{Content: &reply}
		}
	}
	submit := func(args map[string]any) {
		args["token"], args["idempotency_key"] = tok, s.key()
		s.mcp.must("submit_result", args)
	}
	switch {
	case strings.HasPrefix(cmd, "create the "):
		if !strings.Contains(m.Content, "This command takes no result") {
			s.t.Errorf("skill command: %q", m.Content)
		}
	case strings.HasPrefix(cmd, "plan phase 1"):
		ctxv := s.mcp.must("get_context", map[string]any{"token": tok})
		if w, _ := ctxv["wbs"].(string); !strings.HasPrefix(w, "# WBS phase 1") {
			s.t.Errorf("get_context wbs %q", w)
		}
		var plan map[string]any
		_ = json.Unmarshal([]byte(s.r.planJSON(p16Unit{key: "A", manual: true}, p16Unit{key: "B", deps: []string{"A"}})), &plan)
		submit(map[string]any{"status": "COMPLETE", "summary": "planned phase 1", "plan": plan})
	case cmd == "have the project manager perform acceptance tasks":
		submit(map[string]any{"status": "COMPLETE", "summary": "accepted"})
	case cmd == "continue":
		submit(map[string]any{"status": "COMPLETE", "summary": "continued"})
	case strings.HasPrefix(cmd, "implement task "):
		key := strings.TrimPrefix(cmd, "implement task ")
		_ = os.WriteFile(filepath.Join(s.r.dir, "src", key+".txt"), []byte("implemented "+key+"\n"), 0o644)
		s.mcp.must("report_progress", map[string]any{"token": tok, "idempotency_key": s.key(), "message": "implementing " + key})
		submit(map[string]any{"status": "COMPLETE", "summary": "implemented " + key})
	case strings.HasPrefix(cmd, "test task "):
		key := strings.TrimPrefix(cmd, "test task ")
		s.tested[key]++
		if s.silentTests > 0 {
			s.silentTests--
			break
		}
		task := s.mcp.must("get_task", map[string]any{"token": tok})
		checks, _ := task["acceptance_checks"].([]any)
		met := make([]bool, len(checks))
		status := "COMPLETE"
		for i := range met {
			met[i] = true
		}
		if key == s.failFirstTest && s.tested[key] == 1 {
			met[0], status = false, "INCOMPLETE"
		}
		submit(map[string]any{"status": status, "summary": "tested " + key, "criteria_met": met, "evidence": map[string]any{"output": "ok"}})
	default:
		s.t.Errorf("unexpected command %q", m.Content)
	}
	reply := "done: " + cmd
	return &hermestest.Behavior{Content: &reply}
}

// p19mcpRig builds a statefile-workflow project with the MCP team
// answering.
func p19mcpRig(t *testing.T) (*p16Rig, *mcpTeam) {
	t.Helper()
	r := p16NewRigWith(t, p16Options{statefile: true})
	c, err := p19mcpConnect(t, r.h.App.MCPServer().Addr(), p19mcpSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	team := &mcpTeam{t: t, r: r, mcp: c, tested: map[string]int{}}
	r.g.Responder = team.respond
	return r, team
}

// T12.1 / T12.2 / T7.4 / T8.1 — the statefile workflow end to end: every
// result arrives through MCP, every transition is decided from the
// database, the tasks run in dependency order, and no statefile is read
// or written.
func TestP19MCPDatabaseWorkflow(t *testing.T) {
	r, team := p19mcpRig(t)
	team.failFirstTest = "A"
	ctx := context.Background()

	guide, err := os.ReadFile(filepath.Join(r.dir, service.GuideFile))
	if err != nil || !strings.Contains(string(guide), "through the overseer MCP server") || strings.Contains(string(guide), "BACKLOG.md") {
		t.Fatalf("HERMES.md: %s %v", guide, err)
	}
	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"have the project manager perform acceptance tasks", // loads A
		"implement task A", "test task A",                   // a check unmet
		"implement task A", "test task A",
		"have the project manager perform acceptance tasks", // archives A, loads B
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks", // archives B
	}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	// Every command carried its own token.
	seen := map[string]bool{}
	for _, tok := range team.tokens {
		if seen[tok] {
			t.Fatalf("token %s reused across commands", tok)
		}
		seen[tok] = true
	}
	where := "project_id = '" + r.pid.String() + "'"
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND w.archived_at IS NOT NULL AND w.archived_by_dispatch_id IS NOT NULL AND p.phase_id = '"+r.phases[1].String()+"'"); n != 2 {
		t.Fatalf("%d units DONE and archived, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM task_reports WHERE "+where); n != 10 {
		t.Fatalf("%d task reports, want 10", n)
	}
	if n := r.count(t, "SELECT count(*) FROM task_reports WHERE kind = 'TEST' AND status = 'INCOMPLETE' AND "+where); n != 1 {
		t.Fatalf("%d incomplete tests, want 1", n)
	}
	if n := r.count(t, "SELECT count(*) FROM acceptance_approvals WHERE approved_by = 'TESTER'"); n < 1 {
		t.Fatalf("%d tester approvals", n)
	}
	evs := r.events(t)
	if countOf(evs, "MCP_RESULT") != 10 || countOf(evs, "MCP_PROGRESS") != 3 || countOf(evs, "MCP_RESULT_MISSING") != 0 {
		t.Fatalf("events %v", evs)
	}
	// No statefile decided anything: the team files are the only ones.
	assertNoStatefiles(t, r.dir)
	if n := r.count(t, "SELECT count(*) FROM statefile_hashes WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d statefiles verified", n)
	}
}

// assertNoStatefiles fails when the working directory holds a retired
// statefile or the WBS folder: only the generated team files remain.
func assertNoStatefiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, wbs.StatefilesDir))
	if err != nil {
		t.Fatal(err)
	}
	retired := map[string]bool{"PLAN.md": true, "BACKLOG.md": true, "ACTIVE_TASK.md": true, "TASK_ARCHIVE.md": true,
		"TASK_ANALYSIS.md": true, "PLAN_REJECTED.md": true}
	for _, e := range entries {
		if e.IsDir() || retired[e.Name()] {
			t.Fatalf("statefiles/%s exists", e.Name())
		}
	}
}
