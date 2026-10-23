package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillsTools(t *testing.T) {
	// Setup temporary directory with test skills
	tmpDir := t.TempDir()
	t.Setenv("SKILLS_PATH", tmpDir)

	skill1Dir := filepath.Join(tmpDir, "test-skill-1")
	if err := os.MkdirAll(skill1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	skill1Content := `---
name: test-skill-1
description: First test skill playbook
---
# Test Skill 1
Instructions here.
`
	if err := os.WriteFile(filepath.Join(skill1Dir, "SKILL.md"), []byte(skill1Content), 0644); err != nil {
		t.Fatal(err)
	}

	skill2Dir := filepath.Join(tmpDir, "test-skill-2")
	if err := os.MkdirAll(skill2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	skill2Content := `---
name: test-skill-2
description: Second test skill playbook
---
# Test Skill 2
`
	if err := os.WriteFile(filepath.Join(skill2Dir, "SKILL.md"), []byte(skill2Content), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Test ListSkillsTool
	listTool := NewListSkillsTool()
	if listTool.Name() != "list_skills" {
		t.Fatalf("unexpected name: %s", listTool.Name())
	}
	if err := listTool.Validate(nil); err != nil {
		t.Fatalf("unexpected validate err: %v", err)
	}
	res, err := listTool.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected execute err: %v", err)
	}

	var listOut struct {
		Skills []SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal([]byte(res), &listOut); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}
	if len(listOut.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(listOut.Skills))
	}
	if listOut.Skills[0].Name != "test-skill-1" || listOut.Skills[0].Description != "First test skill playbook" {
		t.Errorf("unexpected skill 0: %+v", listOut.Skills[0])
	}
	if listOut.Skills[1].Name != "test-skill-2" || listOut.Skills[1].Description != "Second test skill playbook" {
		t.Errorf("unexpected skill 1: %+v", listOut.Skills[1])
	}

	// 2. Test ReadSkillTool
	readTool := NewReadSkillTool()
	if readTool.Name() != "read_skill" {
		t.Fatalf("unexpected name: %s", readTool.Name())
	}

	// Valid skill read
	readArgs, _ := json.Marshal(map[string]string{"name": "test-skill-1"})
	if err := readTool.Validate(readArgs); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	readRes, err := readTool.Execute(ctx, readArgs)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	var readOut struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(readRes), &readOut); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if readOut.Name != "test-skill-1" || readOut.Content != skill1Content {
		t.Errorf("unexpected read content: %+v", readOut)
	}

	// Invalid skill name validation
	badArgs := []byte(`{"name": "../escape"}`)
	if err := readTool.Validate(badArgs); err == nil {
		t.Error("expected validation error for path traversal attempt")
	}

	// Nonexistent skill
	nonexistentArgs := []byte(`{"name": "does-not-exist"}`)
	if _, err := readTool.Execute(ctx, nonexistentArgs); err == nil {
		t.Error("expected execute error for nonexistent skill")
	}
}
