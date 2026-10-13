package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// ResultTarget is caller authority for one existing paragraph block, not Notion CAS.
type ResultTarget struct {
	Reference, Project, Workspace, Task, Page, Block string
}

type ResultDelivery struct {
	target ResultTarget
	token  string
	client *http.Client
}

type resultEnvelope struct {
	Version      int
	Operation    string
	TargetDigest string
	Handoff      ResultHandoff
	Payload      []byte
}

const resultOperation = "block-rich-text-replace"

// BindingDigest pins public writer authority and the supported operation, never a token.
func (d *ResultDelivery) BindingDigest() string {
	if d == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		Target    ResultTarget
		Operation string
	}{d.target, resultOperation})
	return core.Digest(string(b))
}

func (d *ResultDelivery) MatchesBinding(c core.Contract) bool {
	return d != nil && d.target.Project == c.ProjectID && d.target.Workspace == c.Workspace &&
		d.target.Task == c.TaskID && d.target.Page == c.PageID
}

// No token discovery, default transport, target creation, append or automatic retry.
func NewResultDelivery(target ResultTarget, token string, transport http.RoundTripper) (*ResultDelivery, error) {
	root, err := config.ResolveWorkspace(target.Workspace)
	if err != nil || root != target.Workspace || !claimText(target.Reference) || !claimText(target.Project) ||
		!claimText(target.Task) || !uuid.MatchString(target.Page) || !uuid.MatchString(target.Block) ||
		!claimText(token) || transport == nil {
		return nil, ErrResult
	}
	return &ResultDelivery{target: target, token: token, client: &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (d *ResultDelivery) envelope(h ResultHandoff, payload []byte) ([]byte, error) {
	if d == nil || h.Binding.ProjectID != d.target.Project || h.Binding.Workspace != d.target.Workspace ||
		h.Binding.TaskID != d.target.Task || h.Binding.PageID != d.target.Page ||
		ConfirmResultReadback(h, h) != nil || core.Digest(string(payload)) != h.PayloadDigest {
		return nil, ErrResult
	}
	b, err := json.Marshal(resultEnvelope{1, resultOperation, d.BindingDigest(), h, payload})
	if err != nil || len(b) > 2000 {
		return nil, ErrResult
	}
	return b, nil
}

func (d *ResultDelivery) exchange(ctx context.Context, method string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://api.notion.com/v1/blocks/"+d.target.Block, bytes.NewReader(body))
	if err != nil {
		return nil, ErrResult
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Notion-Version", "2026-03-11")
	req.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(req)
	if err != nil {
		return nil, ErrResult
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || response.StatusCode != http.StatusOK || len(b) > 64*1024 {
		return nil, ErrResult
	}
	return b, nil
}

// Observe distinguishes an empty sole-writer slot from matching or conflicting data.
func (d *ResultDelivery) Observe(ctx context.Context, h ResultHandoff, payload []byte) (bool, bool, error) {
	expected, err := d.envelope(h, payload)
	if err != nil {
		return false, false, err
	}
	b, err := d.exchange(ctx, http.MethodGet, nil)
	if err != nil {
		return false, false, err
	}
	var block struct {
		Object   string          `json:"object"`
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		InTrash  *bool           `json:"in_trash"`
		Archived json.RawMessage `json:"archived"`
		Parent   struct {
			Type string `json:"type"`
			Page string `json:"page_id"`
		} `json:"parent"`
		Paragraph struct {
			Text []struct {
				Plain *string `json:"plain_text"`
			} `json:"rich_text"`
		} `json:"paragraph"`
	}
	if decodeJSON(b, &block) != nil || block.Object != "block" || block.ID != d.target.Block || block.Type != "paragraph" ||
		block.Parent.Type != "page_id" || block.Parent.Page != d.target.Page || block.Paragraph.Text == nil ||
		block.InTrash == nil || *block.InTrash {
		return false, false, ErrResult
	}
	// Archived is a deprecated optional alias; a supplied value must still be typed false.
	if len(block.Archived) != 0 {
		var archived *bool
		if json.Unmarshal(block.Archived, &archived) != nil || archived == nil || *archived {
			return false, false, ErrResult
		}
	}
	if len(block.Paragraph.Text) == 0 {
		return false, true, nil
	}
	if len(block.Paragraph.Text) != 1 || block.Paragraph.Text[0].Plain == nil {
		return false, false, ErrResult
	}
	if !bytes.Equal([]byte(*block.Paragraph.Text[0].Plain), expected) {
		return false, false, ErrResult
	}
	return true, false, nil
}

// Replace sends one supported PATCH. Local singleton exclusion is not an API precondition.
func (d *ResultDelivery) Replace(ctx context.Context, h ResultHandoff, payload []byte) error {
	b, err := d.envelope(h, payload)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"paragraph": map[string]any{"rich_text": []any{map[string]any{"type": "text", "text": map[string]string{"content": string(b)}}}}})
	_, err = d.exchange(ctx, http.MethodPatch, body)
	return err
}
