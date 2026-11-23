package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type fileTool struct {
	info *schema.ToolInfo
	run  func(args json.RawMessage) (string, error)
}

func (t *fileTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *fileTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	raw := json.RawMessage(strings.TrimSpace(argumentsInJSON))
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	result, err := t.run(raw)
	if err != nil {
		return toolError(err), nil
	}
	return result, nil
}

func toolError(err error) string {
	return "error: " + err.Error()
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func requireString(value *string, field string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("missing required argument %q", field)
	}
	return *value, nil
}

func nameParam() *schema.ParameterInfo {
	return &schema.ParameterInfo{
		Type:     schema.String,
		Desc:     "File name: ORIGINAL.txt, CURRENT.txt, or SCRATCH-<n>.txt.",
		Required: true,
	}
}

func NewTools(overlay *Overlay) []tool.BaseTool {
	return []tool.BaseTool{
		listFilesTool(overlay),
		readFileTool(overlay),
		writeFileTool(overlay),
		editFileTool(overlay),
		grepTool(overlay),
	}
}

func listFilesTool(overlay *Overlay) *fileTool {
	return &fileTool{
		info: &schema.ToolInfo{
			Name: "list_files",
			Desc: "List the session files, one per line, each followed by its size in bytes.",
		},
		run: func(json.RawMessage) (string, error) {
			names, err := overlay.Names()
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, name := range names {
				content, err := overlay.Read(name)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&b, "%s %d\n", name, len(content))
			}
			return b.String(), nil
		},
	}
}

func readFileTool(overlay *Overlay) *fileTool {
	return &fileTool{
		info: &schema.ToolInfo{
			Name: "read_file",
			Desc: "Return the full contents of a session file.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"name": nameParam(),
			}),
		},
		run: func(raw json.RawMessage) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			return overlay.Read(args.Name)
		},
	}
}

func writeFileTool(overlay *Overlay) *fileTool {
	return &fileTool{
		info: &schema.ToolInfo{
			Name: "write_file",
			Desc: "Replace the full contents of a session file. SCRATCH-<n>.txt files are created when absent. ORIGINAL.txt is read-only.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"name":    nameParam(),
				"content": {Type: schema.String, Desc: "New full contents of the file.", Required: true},
			}),
		},
		run: func(raw json.RawMessage) (string, error) {
			var args struct {
				Name    string  `json:"name"`
				Content *string `json:"content"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			if err := checkWritable(args.Name); err != nil {
				return "", err
			}
			content, err := requireString(args.Content, "content")
			if err != nil {
				return "", err
			}
			if err := overlay.Write(args.Name, content); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(content), args.Name), nil
		},
	}
}

func countOccurrences(haystack, needle string) int {
	count := 0
	for offset := 0; ; {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return count
		}
		count++
		offset += index + 1
	}
}

func editFileTool(overlay *Overlay) *fileTool {
	return &fileTool{
		info: &schema.ToolInfo{
			Name: "edit_file",
			Desc: "Replace the single occurrence of old_string in a session file with new_string. The edit is refused when old_string is empty or does not occur exactly once. ORIGINAL.txt is read-only.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"name":       nameParam(),
				"old_string": {Type: schema.String, Desc: "Exact text to replace. Must occur exactly once.", Required: true},
				"new_string": {Type: schema.String, Desc: "Replacement text.", Required: true},
			}),
		},
		run: func(raw json.RawMessage) (string, error) {
			var args struct {
				Name      string  `json:"name"`
				OldString *string `json:"old_string"`
				NewString *string `json:"new_string"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			if err := checkWritable(args.Name); err != nil {
				return "", err
			}
			oldString, err := requireString(args.OldString, "old_string")
			if err != nil {
				return "", err
			}
			newString, err := requireString(args.NewString, "new_string")
			if err != nil {
				return "", err
			}
			if oldString == "" {
				return "", errors.New("old_string must not be empty")
			}
			content, err := overlay.Read(args.Name)
			if err != nil {
				return "", err
			}
			if n := countOccurrences(content, oldString); n != 1 {
				return "", fmt.Errorf("old_string must occur exactly once in %s, but %d occurrences were found", args.Name, n)
			}
			if err := overlay.Write(args.Name, strings.Replace(content, oldString, newString, 1)); err != nil {
				return "", err
			}
			return "edited " + args.Name, nil
		},
	}
}

func grepTool(overlay *Overlay) *fileTool {
	return &fileTool{
		info: &schema.ToolInfo{
			Name: "grep",
			Desc: "Search session files with a Go RE2 regular expression. Matches are returned one per line as name:line:text with 1-based line numbers. When name is omitted, every session file is searched.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"pattern": {Type: schema.String, Desc: "Go RE2 regular expression.", Required: true},
				"name":    {Type: schema.String, Desc: "Optional file name to restrict the search to."},
			}),
		},
		run: func(raw json.RawMessage) (string, error) {
			var args struct {
				Pattern *string `json:"pattern"`
				Name    *string `json:"name"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			pattern, err := requireString(args.Pattern, "pattern")
			if err != nil {
				return "", err
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %w", err)
			}

			var names []string
			if args.Name != nil && *args.Name != "" {
				if err := checkName(*args.Name); err != nil {
					return "", err
				}
				names = []string{*args.Name}
			} else {
				names, err = overlay.Names()
				if err != nil {
					return "", err
				}
			}

			var b strings.Builder
			for _, name := range names {
				content, err := overlay.Read(name)
				if err != nil {
					return "", err
				}
				for i, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
					if re.MatchString(line) {
						fmt.Fprintf(&b, "%s:%d:%s\n", name, i+1, line)
					}
				}
			}
			if b.Len() == 0 {
				return "no matches", nil
			}
			return b.String(), nil
		},
	}
}
