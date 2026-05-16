package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchArgs struct {
	WorkspacePath string `json:"workspace_path" jsonschema:"description=Absolute path to the workspace root to search."`
	Query         string `json:"query,omitempty" jsonschema:"description=Optional substring filter for symbol name, file path, or source snippet."`
}

type GenerateTagsArgs struct {
	WorkspacePath string `json:"workspace_path" jsonschema:"description=Absolute path to the workspace root to scan."`
	OutputPath    string `json:"output_path,omitempty" jsonschema:"description=Optional output path for the generated tags file. Relative paths are resolved against workspace_path; defaults to workspace_path/tags."`
}

type CtagsResult struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	Code   string `json:"code,omitempty"`
}

type GenerateTagsResult struct {
	WorkspacePath string `json:"workspace_path"`
	TagsPath      string `json:"tags_path"`
	FileCount     int    `json:"file_count"`
}

type ctagsTag struct {
	Name    string
	File    string
	Pattern string
}

type fileCacheEntry struct {
	lines []string
	err   error
}

const (
	ctagsBatchSize = 250
)

func main() {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "go-ctags-server",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_symbols",
		Description: "Search for code symbols in a workspace directory using ctags.",
	}, handleSearchSymbols)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_tags",
		Description: "Generate a tags file for a workspace directory using ctags.",
	}, handleGenerateTags)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "Server runtime error: %v\n", err)
		os.Exit(1)
	}
}

func handleSearchSymbols(ctx context.Context, _ *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, []CtagsResult, error) {
	workspacePath := strings.TrimSpace(args.WorkspacePath)
	if workspacePath == "" {
		return nil, nil, fmt.Errorf("workspace_path is a required field")
	}

	absWorkspacePath, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving workspace_path: %w", err)
	}

	info, err := os.Stat(absWorkspacePath)
	if err != nil {
		return nil, nil, fmt.Errorf("checking workspace_path: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("workspace_path must be a directory: %s", absWorkspacePath)
	}

	files, err := collectWorkspaceFiles(absWorkspacePath)
	if err != nil {
		return nil, nil, err
	}

	cache := make(map[string]fileCacheEntry)
	var matches []CtagsResult
	query := strings.TrimSpace(args.Query)

	for start := 0; start < len(files); start += ctagsBatchSize {
		end := start + ctagsBatchSize
		if end > len(files) {
			end = len(files)
		}

		tags, err := runCtagsBatch(ctx, files[start:end])
		if err != nil {
			return nil, nil, err
		}

		for _, tag := range tags {
			if !matchesQuery(tag, query) {
				continue
			}

			relPath := tag.File
			if rel, relErr := filepath.Rel(absWorkspacePath, tag.File); relErr == nil && !strings.HasPrefix(rel, "..") {
				relPath = rel
			}

			line, code := resolveSnippet(tag.File, tag.Pattern, cache)
			result := CtagsResult{
				Symbol: tag.Name,
				File:   relPath,
				Line:   line,
				Code:   code,
			}
			matches = append(matches, result)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		if matches[i].Line != matches[j].Line {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].Symbol < matches[j].Symbol
	})

	return nil, matches, nil
}

func handleGenerateTags(ctx context.Context, _ *mcp.CallToolRequest, args GenerateTagsArgs) (*mcp.CallToolResult, GenerateTagsResult, error) {
	workspacePath := strings.TrimSpace(args.WorkspacePath)
	if workspacePath == "" {
		return nil, GenerateTagsResult{}, fmt.Errorf("workspace_path is a required field")
	}

	absWorkspacePath, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, GenerateTagsResult{}, fmt.Errorf("resolving workspace_path: %w", err)
	}

	info, err := os.Stat(absWorkspacePath)
	if err != nil {
		return nil, GenerateTagsResult{}, fmt.Errorf("checking workspace_path: %w", err)
	}
	if !info.IsDir() {
		return nil, GenerateTagsResult{}, fmt.Errorf("workspace_path must be a directory: %s", absWorkspacePath)
	}

	outputPath, err := resolveTagsOutputPath(absWorkspacePath, strings.TrimSpace(args.OutputPath))
	if err != nil {
		return nil, GenerateTagsResult{}, err
	}

	files, err := collectWorkspaceFiles(absWorkspacePath)
	if err != nil {
		return nil, GenerateTagsResult{}, err
	}
	files = excludeFiles(files, outputPath)
	if err := writeTagsFile(ctx, outputPath, files); err != nil {
		return nil, GenerateTagsResult{}, err
	}

	displayPath := outputPath
	if rel, relErr := filepath.Rel(absWorkspacePath, outputPath); relErr == nil && !strings.HasPrefix(rel, "..") {
		displayPath = rel
	}

	return nil, GenerateTagsResult{
		WorkspacePath: absWorkspacePath,
		TagsPath:      displayPath,
		FileCount:     len(files),
	}, nil
}

func collectWorkspaceFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func excludeFiles(files []string, excluded ...string) []string {
	if len(files) == 0 || len(excluded) == 0 {
		return files
	}

	skip := make(map[string]struct{}, len(excluded))
	for _, path := range excluded {
		skip[filepath.Clean(path)] = struct{}{}
	}

	filtered := make([]string, 0, len(files))
	for _, path := range files {
		if _, ok := skip[filepath.Clean(path)]; ok {
			continue
		}
		filtered = append(filtered, path)
	}
	return filtered
}

func shouldSkipDir(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "dist", "build", "out", "target", "coverage", "__pycache__":
		return true
	default:
		return false
	}
}

func runCtagsBatch(ctx context.Context, files []string) ([]ctagsTag, error) {
	output, err := runCtagsBatchOutput(ctx, files)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var tags []ctagsTag
	for scanner.Scan() {
		tag, ok := parseTagLine(scanner.Text())
		if !ok {
			continue
		}
		tags = append(tags, tag)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading ctags output: %w", err)
	}

	return tags, nil
}

func runCtagsBatchOutput(ctx context.Context, files []string) ([]byte, error) {
	if len(files) == 0 {
		return nil, nil
	}

	args := append([]string{"-f", "-"}, files...)
	cmd := exec.CommandContext(ctx, "ctags", args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr == "" {
				stderr = err.Error()
			}
			return nil, fmt.Errorf("ctags failed: %s", stderr)
		}
		return nil, fmt.Errorf("running ctags: %w", err)
	}

	return output, nil
}

func parseTagLine(line string) (ctagsTag, bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 2 {
		return ctagsTag{}, false
	}

	tag := ctagsTag{
		Name: strings.TrimSpace(parts[0]),
		File: strings.TrimSpace(parts[1]),
	}
	if len(parts) == 3 {
		tag.Pattern = strings.TrimSpace(parts[2])
	}
	if tag.Name == "" || tag.File == "" {
		return ctagsTag{}, false
	}
	return tag, true
}

func matchesQuery(tag ctagsTag, query string) bool {
	if query == "" {
		return true
	}

	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(tag.Name), q) ||
		strings.Contains(strings.ToLower(tag.File), q) ||
		strings.Contains(strings.ToLower(normalizePattern(tag.Pattern)), q)
}

func normalizePattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	if strings.HasPrefix(pattern, "/^") && strings.HasSuffix(pattern, "$/") {
		pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/^"), "$/")
	} else if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		pattern = strings.TrimPrefix(strings.TrimSuffix(pattern, "/"), "/")
	}
	pattern = strings.TrimPrefix(pattern, "^")
	pattern = strings.TrimSuffix(pattern, "$")
	pattern = strings.ReplaceAll(pattern, `\/`, `/`)
	pattern = strings.ReplaceAll(pattern, `\\`, `\`)
	return pattern
}

func resolveSnippet(filePath, pattern string, cache map[string]fileCacheEntry) (int, string) {
	snippet := normalizePattern(pattern)
	if snippet == "" {
		return 0, ""
	}

	entry, ok := cache[filePath]
	if !ok {
		lines, err := readFileLines(filePath)
		entry = fileCacheEntry{lines: lines, err: err}
		cache[filePath] = entry
	}
	if entry.err != nil {
		return 0, snippet
	}

	for i, line := range entry.lines {
		if line == snippet {
			return i + 1, line
		}
	}
	return 0, snippet
}

func resolveTagsOutputPath(workspacePath, requested string) (string, error) {
	if requested == "" {
		return filepath.Join(workspacePath, "tags"), nil
	}

	if !filepath.IsAbs(requested) {
		requested = filepath.Join(workspacePath, requested)
	}

	return filepath.Abs(requested)
}

func writeTagsFile(ctx context.Context, outputPath string, files []string) (retErr error) {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tags-*")
	if err != nil {
		return fmt.Errorf("creating temporary tags file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.WriteString("!_TAG_FILE_FORMAT\t2\t/extended format; --format=1 will not append field names/\n"); err != nil {
		retErr = fmt.Errorf("writing tags header: %w", err)
		return retErr
	}
	if _, err := tmp.WriteString("!_TAG_FILE_SORTED\t0\t/0=unsorted, 1=sorted/\n"); err != nil {
		retErr = fmt.Errorf("writing tags header: %w", err)
		return retErr
	}
	if _, err := tmp.WriteString("!_TAG_PROGRAM_NAME\tgo-ctags-mcp\t//\n"); err != nil {
		retErr = fmt.Errorf("writing tags header: %w", err)
		return retErr
	}

	for start := 0; start < len(files); start += ctagsBatchSize {
		end := start + ctagsBatchSize
		if end > len(files) {
			end = len(files)
		}

		output, err := runCtagsBatchOutput(ctx, files[start:end])
		if err != nil {
			retErr = err
			return retErr
		}
		if len(output) == 0 {
			continue
		}
		if _, err := tmp.Write(output); err != nil {
			retErr = fmt.Errorf("writing tags data: %w", err)
			return retErr
		}
		if output[len(output)-1] != '\n' {
			if _, err := tmp.WriteString("\n"); err != nil {
				retErr = fmt.Errorf("writing tags data: %w", err)
				return retErr
			}
		}
	}

	if err := tmp.Close(); err != nil {
		retErr = fmt.Errorf("closing tags file: %w", err)
		return retErr
	}

	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		retErr = fmt.Errorf("removing existing tags file: %w", err)
		return retErr
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		retErr = fmt.Errorf("moving tags file into place: %w", err)
		return retErr
	}

	return nil
}

func readFileLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	return strings.Split(text, "\n"), nil
}
