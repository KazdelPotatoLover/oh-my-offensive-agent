package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	validSkillNameRx = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)
	frontmatterRx    = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n`)
	nameFieldRx      = regexp.MustCompile(`(?m)^name:\s*['"]?([^\r\n'"]+)['"]?`)
	descFieldRx      = regexp.MustCompile(`(?m)^description:\s*['"]?([^\r\n'"]+)['"]?`)
)

func resolveSkillsDir() string {
	if p := os.Getenv("SKILLS_PATH"); p != "" {
		return p
	}
	for _, candidate := range []string{"skills", "../skills", "../../skills", "/app/skills"} {
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate
		}
	}
	return "skills"
}

type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ListSkillsTool struct{}

func NewListSkillsTool() *ListSkillsTool {
	return &ListSkillsTool{}
}

func (t *ListSkillsTool) Name() string { return "list_skills" }
func (t *ListSkillsTool) Description() string {
	return "List available domain skills and security testing playbooks (e.g. 'playwright-cli', 'api-recon', 'finding-verification')."
}
func (t *ListSkillsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *ListSkillsTool) Validate(args []byte) error { return nil }
func (t *ListSkillsTool) Execute(ctx context.Context, args []byte) (string, error) {
	skillsDir := resolveSkillsDir()
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return `{"skills":[]}`, nil
		}
		return "", fmt.Errorf("read skills dir: %w", err)
	}

	var skills []SkillMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillFile := filepath.Join(skillsDir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}
		name := e.Name()
		desc := ""
		if m := frontmatterRx.FindSubmatch(data); len(m) > 1 {
			fm := string(m[1])
			if nm := nameFieldRx.FindStringSubmatch(fm); len(nm) > 1 {
				name = strings.TrimSpace(nm[1])
			}
			if dm := descFieldRx.FindStringSubmatch(fm); len(dm) > 1 {
				desc = strings.TrimSpace(dm[1])
			}
		}
		skills = append(skills, SkillMeta{Name: name, Description: desc})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	out, err := json.Marshal(map[string]any{"skills": skills})
	return string(out), err
}

type ReadSkillTool struct{}

func NewReadSkillTool() *ReadSkillTool {
	return &ReadSkillTool{}
}

func (t *ReadSkillTool) Name() string { return "read_skill" }
func (t *ReadSkillTool) Description() string {
	return "Read the detailed instructions and playbooks of a domain skill (e.g. 'playwright-cli', 'api-recon', 'finding-verification')."
}
func (t *ReadSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Name of the skill to read (e.g. 'playwright-cli', 'api-recon', 'finding-verification')"
			}
		},
		"required": ["name"]
	}`)
}
func (t *ReadSkillTool) Validate(args []byte) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if !validSkillNameRx.MatchString(in.Name) {
		return errors.New("skill name must only contain alphanumeric characters, underscores, or hyphens")
	}
	return nil
}
func (t *ReadSkillTool) Execute(ctx context.Context, args []byte) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if !validSkillNameRx.MatchString(in.Name) {
		return "", errors.New("invalid skill name")
	}
	skillsDir := resolveSkillsDir()
	skillPath := filepath.Join(skillsDir, in.Name, "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("skill %q not found", in.Name)
		}
		return "", fmt.Errorf("read skill file: %w", err)
	}
	out, err := json.Marshal(map[string]any{
		"name":    in.Name,
		"content": string(data),
	})
	return string(out), err
}
