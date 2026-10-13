package notion

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// AccessScope is an independently authorized caller input, never read from a Notion page.
type AccessScope struct {
	ProjectID, Workspace, DataSource, AccessRef string
	TaskPages                                   map[string]string
	Pages, Actions                              []string
}

type AccessReader struct {
	reader         *Reader
	taskID, pageID string
}

// NewAccessReader reuses Reader while restricting every GET, including dependency reads.
// A matching reference binds supplied authority; it does not authenticate its issuer.
func NewAccessReader(profile config.Profile, contract core.Contract, scope AccessScope, callerRef string, reader *Reader) (*AccessReader, error) {
	root, err := config.ResolveWorkspace(scope.Workspace)
	if err != nil || root != scope.Workspace || scope.Workspace != profile.WorkspaceRoot || scope.Workspace != contract.Workspace ||
		scope.ProjectID != profile.ProjectID || scope.ProjectID != contract.ProjectID ||
		core.CheckContract(profile, contract, time.Now()) != "CONTRACT_VALID" || reader == nil || reader.token == "" ||
		!uuid.MatchString(scope.DataSource) || reader.dataSource != scope.DataSource ||
		strings.TrimSpace(scope.AccessRef) != scope.AccessRef || scope.AccessRef == "" || strings.ContainsAny(scope.AccessRef, "\r\n") ||
		scope.AccessRef != callerRef || len(scope.Actions) != 1 || scope.Actions[0] != "read" ||
		len(scope.Pages) == 0 || len(scope.Pages) > 64 || len(scope.TaskPages) == 0 || len(scope.TaskPages) > 64 {
		return nil, intake.ErrAccess
	}
	pages := make(map[string]bool, len(scope.Pages))
	for _, id := range scope.Pages {
		if !uuid.MatchString(id) || pages[id] {
			return nil, intake.ErrAccess
		}
		pages[id] = true
	}
	for task, page := range scope.TaskPages {
		if !slices.Contains(profile.TaskAllowlist, task) || !pages[page] {
			return nil, intake.ErrAccess
		}
	}
	if scope.TaskPages[contract.TaskID] != contract.PageID {
		return nil, intake.ErrAccess
	}
	clone := *reader
	client := *reader.client
	client.Transport = accessTransport{reader.client.Transport, pages}
	clone.client = &client
	return &AccessReader{reader: &clone, taskID: contract.TaskID, pageID: contract.PageID}, nil
}

func (r *AccessReader) Read(ctx context.Context, id string) (core.TaskSnapshot, error) {
	if r == nil || id != r.pageID {
		return core.TaskSnapshot{}, intake.ErrAccess
	}
	if err := ctx.Err(); err != nil {
		return core.TaskSnapshot{}, err
	}
	task, err := r.reader.Read(ctx, id)
	if err == nil && (task.TaskID != r.taskID || task.PageID != r.pageID) {
		return core.TaskSnapshot{}, intake.ErrSchema
	}
	return task, err
}

// Official page updates have no qualified conditional claim/fencing contract here.
func (*AccessReader) Claim(context.Context, string, core.Claim) error { return intake.ErrClaim }

type accessTransport struct {
	next  http.RoundTripper
	pages map[string]bool
}

func (a accessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "api.notion.com" ||
		req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" || req.URL.Opaque != "" || req.URL.RawPath != "" {
		return nil, intake.ErrAccess
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/pages/"), "/")
	if !strings.HasPrefix(req.URL.Path, "/v1/pages/") || len(parts) < 1 || len(parts) > 2 ||
		!a.pages[parts[0]] || (len(parts) == 2 && parts[1] != "markdown") {
		return nil, intake.ErrAccess
	}
	return a.next.RoundTrip(req)
}
