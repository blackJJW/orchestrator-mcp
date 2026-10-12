package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/core"
)

type Fields struct {
	TaskID, Status, AssignedTo, Owner, LeaseUntil, Base, DependsOn, Checkpoint, Prefix string
}

func MTTFields() Fields {
	return Fields{"Task ID", "Status", "Assigned To", "Owner", "Lease Until", "Git Base SHA", "Depends On", "Checkpoint", "MTT"}
}

type Reader struct {
	token, dataSource string
	fields            Fields
	client            *http.Client
}

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func NewReader(token, dataSource string, fields Fields, transport http.RoundTripper) (*Reader, error) {
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, intake.ErrAccess
	}
	if !uuid.MatchString(dataSource) || fields.TaskID == "" || fields.Status == "" || fields.AssignedTo == "" || fields.Owner == "" || fields.LeaseUntil == "" || fields.Base == "" || fields.DependsOn == "" || fields.Checkpoint == "" || fields.Prefix == "" {
		return nil, intake.ErrSchema
	}
	if transport == nil {
		local := http.DefaultTransport.(*http.Transport).Clone()
		local.Proxy = nil
		transport = local
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Reader{token: token, dataSource: dataSource, fields: fields, client: client}, nil
}

type property struct {
	Type   string `json:"type"`
	Select *struct {
		Name string `json:"name"`
	} `json:"select"`
	UniqueID *struct {
		Number int    `json:"number"`
		Prefix string `json:"prefix"`
	} `json:"unique_id"`
	Date     json.RawMessage `json:"date"`
	RichText []struct {
		PlainText *string `json:"plain_text"`
	} `json:"rich_text"`
	Relation []struct {
		ID string `json:"id"`
	} `json:"relation"`
	HasMore *bool `json:"has_more"`
}

type page struct {
	Object   string `json:"object"`
	ID       string `json:"id"`
	Archived *bool  `json:"archived"`
	InTrash  *bool  `json:"in_trash"`
	Parent   struct {
		DataSourceID string `json:"data_source_id"`
	} `json:"parent"`
	Properties map[string]property `json:"properties"`
}

func (r *Reader) page(ctx context.Context, id string) (page, error) {
	var value page
	if !uuid.MatchString(id) {
		return value, intake.ErrSchema
	}
	if err := r.get(ctx, "pages/"+id, &value); err != nil {
		return value, err
	}
	if value.Object != "page" || value.ID != id || value.Parent.DataSourceID != r.dataSource || value.Archived == nil || value.InTrash == nil || *value.Archived || *value.InTrash {
		return value, intake.ErrSchema
	}
	return value, nil
}

func selected(p property) (string, error) {
	if p.Type != "select" || p.Select == nil || p.Select.Name == "" {
		return "", intake.ErrSchema
	}
	return p.Select.Name, nil
}

func text(p property) (string, error) {
	if p.Type != "rich_text" || p.RichText == nil {
		return "", intake.ErrSchema
	}
	var value strings.Builder
	for _, part := range p.RichText {
		if part.PlainText == nil {
			return "", intake.ErrSchema
		}
		value.WriteString(*part.PlainText)
	}
	return value.String(), nil
}

func (r *Reader) Read(ctx context.Context, id string) (core.TaskSnapshot, error) {
	var task core.TaskSnapshot
	p, err := r.page(ctx, id)
	if err != nil {
		return task, err
	}
	f := r.fields
	prop := p.Properties[f.TaskID]
	if prop.Type != "unique_id" || prop.UniqueID == nil || prop.UniqueID.Number <= 0 || prop.UniqueID.Prefix != f.Prefix {
		return task, intake.ErrSchema
	}
	task.TaskID, task.PageID = fmt.Sprintf("%s-%d", f.Prefix, prop.UniqueID.Number), id
	status, err := selected(p.Properties[f.Status])
	if err != nil {
		return task, err
	}
	switch status {
	case "BACKLOG", "READY_FOR_IMPLEMENTATION", "CHANGES_REQUESTED", "IN_PROGRESS":
		task.Eligible = true
	case "DONE", "BLOCKED", "RESEARCH", "READY_FOR_REVIEW", "WAITING_CAPACITY", "SUSPENDED", "CANCELLED":
	default:
		return task, intake.ErrSchema
	}
	task.Executor, err = selected(p.Properties[f.AssignedTo])
	if err != nil {
		return task, err
	}
	task.Owner, err = selected(p.Properties[f.Owner])
	if err != nil {
		return task, err
	}
	switch task.Owner {
	case "Unclaimed", "Codex", "ChatGPT", "Antigravity", "Human":
	default:
		return task, intake.ErrSchema
	}
	lease := p.Properties[f.LeaseUntil]
	if lease.Type != "date" || len(lease.Date) == 0 {
		return task, intake.ErrSchema
	}
	if string(lease.Date) != "null" {
		var date struct {
			Start string `json:"start"`
		}
		if decodeJSON(lease.Date, &date) != nil {
			return task, intake.ErrSchema
		}
		task.LeaseUntil, err = time.Parse(time.RFC3339, date.Start)
		if err != nil {
			return task, intake.ErrSchema
		}
	}
	if task.Owner != "Unclaimed" && string(lease.Date) == "null" {
		return task, intake.ErrSchema
	}
	task.OwnershipKnown = true
	task.Base, err = text(p.Properties[f.Base])
	if err != nil {
		return task, err
	}
	checkpoint, err := text(p.Properties[f.Checkpoint])
	if err != nil {
		return task, err
	}
	if strings.HasPrefix(checkpoint, "controller-claim:") {
		task.ClaimNonce = strings.TrimPrefix(checkpoint, "controller-claim:")
	}
	deps := p.Properties[f.DependsOn]
	if deps.Type != "relation" || deps.Relation == nil || deps.HasMore == nil || *deps.HasMore || len(deps.Relation) > 25 {
		return task, intake.ErrSchema
	}
	task.DependenciesSatisfied = true
	for _, dep := range deps.Relation {
		other, err := r.page(ctx, dep.ID)
		if err != nil {
			return task, err
		}
		state, err := selected(other.Properties[f.Status])
		if err != nil {
			return task, err
		}
		if state != "DONE" {
			task.DependenciesSatisfied = false
		}
	}
	var markdown struct {
		Object        string            `json:"object"`
		ID            string            `json:"id"`
		Markdown      string            `json:"markdown"`
		Truncated     *bool             `json:"truncated"`
		UnknownBlocks []string          `json:"unknown_block_ids"`
		Warnings      []json.RawMessage `json:"warnings"`
	}
	if err := r.get(ctx, "pages/"+id+"/markdown", &markdown); err != nil {
		return task, err
	}
	if markdown.Object != "page_markdown" || markdown.ID != id || markdown.Truncated == nil || *markdown.Truncated || markdown.UnknownBlocks == nil || len(markdown.UnknownBlocks) > 0 || len(markdown.Warnings) > 0 {
		return task, intake.ErrSchema
	}
	task.Brief, err = Brief(markdown.Markdown)
	return task, err
}

// Brief excludes appended coordination results; only the approved execution contract can enter a prompt.
func Brief(markdown string) (string, error) {
	start := strings.Index(markdown, "Executor role:")
	if start < 0 {
		return "", intake.ErrSchema
	}
	brief := markdown[start:]
	end := strings.Index(brief, "## Expected result / handoff")
	if end < 0 {
		return "", intake.ErrSchema
	}
	if next := strings.Index(brief[end+len("## Expected result / handoff"):], "\n## "); next >= 0 {
		brief = brief[:end+len("## Expected result / handoff")+next]
	}
	for _, heading := range []string{"Mutation authority:", "## Objective", "## Git base & relevant context", "## Required change / investigation", "## Preserve / invariants", "## Out of scope / must not", "## Authorization delta", "## Validation classification", "## Acceptance criteria", "## Stop / block conditions", "## Expected result / handoff"} {
		if strings.Count(brief, heading) != 1 {
			return "", intake.ErrSchema
		}
	}
	return strings.TrimSpace(brief), nil
}

func (r *Reader) Claim(context.Context, string, core.Claim) error { return intake.ErrClaim }

func (r *Reader) get(ctx context.Context, path string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.notion.com/v1/"+path, nil)
	if err != nil {
		return intake.ErrSchema
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Notion-Version", "2026-03-11")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("NOTION_READ_FAILED")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return intake.ErrAccess
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("NOTION_HTTP_%d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return intake.ErrSchema
	}
	return decodeJSON(data, output)
}

func decodeJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if uniqueJSONValue(decoder, reflect.TypeOf(output)) != nil {
		return intake.ErrSchema
	}
	if _, err := decoder.Token(); err != io.EOF || json.Unmarshal(data, output) != nil {
		return intake.ErrSchema
	}
	return nil
}

// Check nested keys before Unmarshal can discard an earlier owner or completeness flag.
func uniqueJSONValue(decoder *json.Decoder, schema reflect.Type) error {
	for schema != nil && schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	token, err := decoder.Token()
	if err != nil {
		return intake.ErrSchema
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		var child reflect.Type
		if delim == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return intake.ErrSchema
			}
			seen[key] = true
			if schema != nil && schema.Kind() == reflect.Struct {
				for i := 0; i < schema.NumField(); i++ {
					field := schema.Field(i)
					name := field.Tag.Get("json")
					if name == "" {
						name = field.Name
					}
					if key == name {
						child = field.Type
						break
					}
					// Unmarshal folds struct keys; reject aliases before they can overwrite a known field.
					if strings.EqualFold(key, name) {
						return intake.ErrSchema
					}
				}
			} else if schema != nil && schema.Kind() == reflect.Map {
				child = schema.Elem()
			}
		} else if schema != nil && schema.Kind() == reflect.Slice {
			child = schema.Elem()
		}
		if err := uniqueJSONValue(decoder, child); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return intake.ErrSchema
	}
	return nil
}
