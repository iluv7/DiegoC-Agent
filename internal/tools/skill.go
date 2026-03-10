package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const skillFileName = "SKILL.md"

// Skill holds parsed skill data from SKILL.md.
type Skill struct {
	Name        string
	Description string
	Content     string
	SkillPath   string // absolute dir of the skill (parent of SKILL.md)
}

// ToPrompt returns the full skill content for the agent (with skill root directory).
func (s *Skill) ToPrompt() string {
	root := s.SkillPath
	if root == "" {
		root = "unknown"
	}
	return fmt.Sprintf("\n# Skill: %s\n\n%s\n\n**Skill Root Directory:** `%s`\n\nAll files and references in this skill are relative to this directory.\n\n---\n\n%s\n",
		s.Name, s.Description, root, s.Content)
}

// SkillLoader discovers and loads skills from a directory.
type SkillLoader struct {
	SkillsDir    string
	loadedSkills map[string]*Skill
}

// NewSkillLoader creates a loader for the given skills directory.
func NewSkillLoader(skillsDir string) *SkillLoader {
	return &SkillLoader{SkillsDir: skillsDir, loadedSkills: make(map[string]*Skill)}
}

// DiscoverSkills finds all SKILL.md under skillsDir and loads them.
func (l *SkillLoader) DiscoverSkills() []*Skill {
	var skills []*Skill
	root := filepath.Clean(l.SkillsDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return skills
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != skillFileName {
			return nil
		}
		skill := l.loadSkill(path)
		if skill != nil {
			skills = append(skills, skill)
			l.loadedSkills[skill.Name] = skill
		}
		return nil
	})
	return skills
}

// loadSkill parses one SKILL.md (YAML frontmatter + content).
func (l *SkillLoader) loadSkill(skillPath string) *Skill {
	data, err := os.ReadFile(skillPath)
	if err != nil {
		return nil
	}
	content := string(data)
	// Frontmatter: ---\n...\n---\nrest
	parts := regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n(.*)$`).FindStringSubmatch(content)
	if len(parts) != 3 {
		return nil
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		return nil
	}
	if fm.Name == "" || fm.Description == "" {
		return nil
	}
	skillContent := strings.TrimSpace(parts[2])
	skillDir := filepath.Dir(skillPath)
	absDir, _ := filepath.Abs(skillDir)
	processed := l.processSkillPaths(skillContent, absDir)
	return &Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Content:     processed,
		SkillPath:   absDir,
	}
}

// processSkillPaths replaces relative paths in content with absolute paths (aligned with Mini-Agent skill_loader._process_skill_paths).
func (l *SkillLoader) processSkillPaths(content string, skillDir string) string {
	// Pattern 1: Directory-based paths (scripts/, references/, assets/)
	re1 := regexp.MustCompile(`(python\s+|` + "`" + `)((?:scripts|references|assets)/[^\s` + "`" + `\)]+)`)
	content = re1.ReplaceAllStringFunc(content, func(m string) string {
		sub := re1.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		absPath := filepath.Join(skillDir, sub[2])
		if _, err := os.Stat(absPath); err == nil {
			return sub[1] + absPath
		}
		return m
	})
	// Pattern 2: "see reference.md", "read forms.md" etc.
	re2 := regexp.MustCompile(`(?i)(see|read|refer to|check)\s+([a-zA-Z0-9_.-]+\\.(?:md|txt|json|yaml))([.,;\\s])`)
	content = re2.ReplaceAllStringFunc(content, func(m string) string {
		sub := re2.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		absPath := filepath.Join(skillDir, sub[2])
		if _, err := os.Stat(absPath); err == nil {
			return sub[1] + " `" + absPath + "` (use read_file to access)" + sub[3]
		}
		return m
	})
	// Pattern 3: Markdown links [text](path) or [text](./path)
	re3 := regexp.MustCompile(`(?i)(?:(Read|See|Check|Refer to|Load|View)\s+)?\[(` + "`" + `?[^` + "`" + `\]]+` + "`" + `?)\]\(((?:\./)?[^)]+\.(?:md|txt|json|yaml|js|py|html))\)`)
	content = re3.ReplaceAllStringFunc(content, func(m string) string {
		sub := re3.FindStringSubmatch(m)
		if len(sub) < 4 {
			return m
		}
		prefix := ""
		if sub[1] != "" {
			prefix = sub[1] + " "
		}
		linkText := sub[2]
		relPath := sub[3]
		if len(relPath) >= 2 && relPath[:2] == "./" {
			relPath = relPath[2:]
		}
		absPath := filepath.Join(skillDir, relPath)
		if _, err := os.Stat(absPath); err == nil {
			return prefix + "[" + linkText + "](`" + absPath + "`) (use read_file to access)"
		}
		return m
	})
	return content
}

// GetSkill returns a loaded skill by name.
func (l *SkillLoader) GetSkill(name string) *Skill {
	return l.loadedSkills[name]
}

// ListSkills returns all loaded skill names.
func (l *SkillLoader) ListSkills() []string {
	names := make([]string, 0, len(l.loadedSkills))
	for n := range l.loadedSkills {
		names = append(names, n)
	}
	return names
}

// GetSkillsMetadataPrompt returns text for system prompt (Progressive Disclosure Level 1).
func (l *SkillLoader) GetSkillsMetadataPrompt() string {
	if len(l.loadedSkills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available Skills\n")
	b.WriteString("You have access to specialized skills. Each skill provides expert guidance for specific tasks.\n")
	b.WriteString("Load a skill's full content using the get_skill tool when needed.\n\n")
	for _, s := range l.loadedSkills {
		b.WriteString(fmt.Sprintf("- `%s`: %s\n", s.Name, s.Description))
	}
	return b.String()
}

// GetSkillTool is a tool that returns full content of a skill by name.
type GetSkillTool struct {
	Loader *SkillLoader
}

func (t *GetSkillTool) Name() string { return "get_skill" }

func (t *GetSkillTool) Description() string {
	return "Get complete content and guidance for a specified skill, used for executing specific types of tasks"
}

func (t *GetSkillTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"skill_name": map[string]interface{}{"type": "string", "description": "Name of the skill to retrieve (use list_skills to view available skills)"},
		},
		"required": []interface{}{"skill_name"},
	}
}

func (t *GetSkillTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	name, _ := args["skill_name"].(string)
	if name == "" {
		return &ToolResult{Success: false, Error: "skill_name is required"}, nil
	}
	skill := t.Loader.GetSkill(name)
	if skill == nil {
		avail := strings.Join(t.Loader.ListSkills(), ", ")
		return &ToolResult{Success: false, Error: fmt.Sprintf("Skill '%s' does not exist. Available skills: %s", name, avail)}, nil
	}
	return &ToolResult{Success: true, Content: skill.ToPrompt()}, nil
}
