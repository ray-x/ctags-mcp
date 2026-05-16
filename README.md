# go-ctags-mcp

A lightweight MCP server that brings `ctags`-powered symbol search to AI coding clients and editors.

Use it to scan a workspace for symbols, inspect matching source snippets, and generate a BSD `ctags` tags file on demand.

## What it does

The server exposes two tools:

- `search_symbols`
- `generate_tags`

Arguments:

- `workspace_path` — absolute path to the project root to search
- `query` — optional filter for symbol name, file path, or source snippet

`generate_tags` writes a BSD `ctags` tags file for the workspace. It defaults to `workspace_path/tags`, and you can override the output path with `output_path`.

## Why choose this over other solutions?

- 🚀 **Zero Dependencies & Instant Setup:** Written in Go and compiled into a single executable binary. No Python environment, node_modules, or shell wrappers required. Just download and run.
- ⚡ **Outpaces Grep & LSPs:** Built specifically for massive, polyglot codebases. While LSPs struggle to initialize across multiple languages and `grep|rg` wastes time brute-forcing text matches, `ctags` indexes structure instantly.
- 🤖 **LLM-Optimized Output:** Designed from the ground up for Model Context Protocol (MCP) clients. It formats code definitions and context snippets natively for LLMs, saving token overhead while maximizing accuracy.

## Requirements

- Go 1.26+
- `ctags` available on `PATH`

## Build

```bash
go build -o go-ctags-mcp .
```

## Run

The server speaks MCP over stdio, so it is usually launched by a client.

## Claude Desktop config

Copy `claude_desktop_config.sample.json` to Claude Desktop's config location and replace the binary path with your local build.

## Copilot config

Copy `copilot-mcp-config.sample.json` to `~/.copilot/mcp-config.json` or the config location used by your Copilot client, then replace the binary path.

## Example usage

```json
{
  "workspace_path": "/Users/you/src/my-project",
  "query": "handler"
}
```
