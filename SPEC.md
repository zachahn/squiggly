# editor: LLM Rewriting CLI and MCP Server

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in BCP 14 (RFC2119) (RFC8174) when, and only when, they appear in all capitals, as shown here.

## 1. Overview

`editor` is a command-line program written in Go. Text is rewritten by an LLM, and the program is exposed to MCP clients as an MCP server over stdio.

Rewriting is performed by an agent loop. The agent is given a sandboxed set of file tools scoped to a per-session state directory. The original text is stored as `ORIGINAL.txt`, and the rewrite is produced in `CURRENT.txt`. A session is started with the original text and MAY be revised any number of times with feedback.

The program MUST be built with:

- `github.com/mark3labs/mcp-go` for the MCP server.
- `github.com/cloudwego/eino` for the agent loop, with chat model components from `github.com/cloudwego/eino-ext`. The choice between eino agent APIs (for example, the ADK `ChatModelAgent` or `flow/agent/react`) is left to the implementation.

### 1.1. Commands

| Command | Purpose |
| --- | --- |
| `editor init` | Interactively create or update the config file. |
| `editor mcp` | Run the MCP server over stdio. |
| `editor prune --older-than <duration>` | Delete sessions older than a given age. |

### 1.2. Supported Platforms

| Platform value | Description | Chat model component |
| --- | --- | --- |
| `ollama` | Ollama native API | eino-ext `ollama` |
| `llamacpp` | llama.cpp `llama-server` OpenAI-compatible API | eino-ext `openai` with a custom base URL |
| `openai-compatible` | Any OpenAI-compatible API (OpenAI, OpenRouter, Groq, LM Studio, vLLM, and others) | eino-ext `openai` with a custom base URL |

The configured model MUST support tool calling. For Ollama, only models with tool support are usable. For llama.cpp, `llama-server` is expected to be started with `--jinja` and a model whose chat template supports tools.

## 2. Configuration

### 2.1. Location

The config file MUST be located at `$XDG_CONFIG_HOME/editor/config.toml`. When `XDG_CONFIG_HOME` is unset or empty, `~/.config/editor/config.toml` MUST be used. This rule applies on every operating system, including macOS.

### 2.2. Format

The config file MUST be TOML with the following keys:

| Key | Type | Required | Description |
| --- | --- | --- | --- |
| `platform` | string | yes | One of `ollama`, `llamacpp`, `openai-compatible`. |
| `base_url` | string | yes | API root. See Section 2.3. |
| `api_key` | string | no | Plaintext API key. Used only by `openai-compatible`. MAY be empty. |
| `model` | string | yes | Model identifier sent to the platform. |
| `system_prompt` | string | yes | Rewriting instructions. See Section 5.1. |
| `max_steps` | integer | no | Maximum agent steps per run. MUST be at least 1. Defaults to 25 when absent. |

### 2.3. Base URL

- For `ollama`, `base_url` is the server root, for example `http://localhost:11434`.
- For `llamacpp` and `openai-compatible`, `base_url` is the OpenAI-style API root including the version segment, for example `http://localhost:8080/v1` or `https://api.openai.com/v1`.

### 2.4. Permissions

The config directory MUST be created with mode `0700` when absent. The config file MUST be written with mode `0600`, because the API key is stored in plaintext.

### 2.5. Example

```toml
platform = "openai-compatible"
base_url = "https://openrouter.ai/api/v1"
api_key = "sk-or-..."
model = "anthropic/claude-sonnet-5.5"
max_steps = 25
system_prompt = """
Rewrite the text for clarity and concision. Preserve the meaning, tone, and any technical details. Do not add new information.
"""
```

## 3. `editor init`

### 3.1. Interactivity

`editor init` MUST collect all inputs through interactive prompts. No flags are defined for config values. When stdin is not a terminal, the command MUST exit with a non-zero status and an error message, without prompting.

### 3.2. Existing Config

When a config file already exists:

1. The user MUST be asked to confirm overwriting the file.
2. When the user declines, the command MUST exit with status 0 and the file MUST be left unchanged.
3. When the user confirms, each subsequent prompt MUST be pre-filled with the value from the existing file. `system_prompt` and `max_steps` MUST be preserved from the existing file without prompting.

When the existing file cannot be parsed, the user MUST still be asked to confirm overwriting it, and prompts MUST fall back to the defaults in Section 3.3.

### 3.3. Prompts

Prompts MUST appear in this order:

1. **Overwrite confirmation.** Shown only when a config file exists.
2. **Platform.** One of `ollama`, `llamacpp`, `openai-compatible`.
3. **Base URL.** The default is `http://localhost:11434` for `ollama` and `http://localhost:8080/v1` for `llamacpp`. No default is offered for `openai-compatible`, and a value MUST be entered.
4. **API key.** Shown only for `openai-compatible`. MAY be left blank.
5. **Model.** See Section 3.4.

### 3.4. Model Discovery

Before the model prompt, the available models MUST be requested from the platform:

- For `ollama`: `GET <base_url>/api/tags`.
- For `llamacpp` and `openai-compatible`: `GET <base_url>/models`. When an API key was entered, the request MUST include `Authorization: Bearer <api_key>`.

When one or more models are returned, a numbered pick list MUST be shown and a model MUST be selected by number. When exactly one model is returned, that model MUST be offered as the default.

When the request fails or returns no models, the reason MUST be printed and the user MUST be asked to type a model name. An empty model name MUST be rejected and the prompt repeated.

### 3.5. Writing

After all prompts are answered, the config file MUST be written as described in Section 2. For a new config, `system_prompt` MUST be set to the default in Section 5.1 and `max_steps` MUST be set to 25. The path of the written file MUST be printed.

## 4. `editor mcp`

### 4.1. Startup

The config MUST be read once at startup and MUST NOT be read again during the life of the process. Edits to the config file take effect only after the server is restarted.

When the config file is missing or invalid (unparseable, unknown `platform`, empty `base_url`, empty `model`, empty `system_prompt`, or `max_steps` below 1), the process MUST exit with a non-zero status and an error message on stderr that suggests running `editor init`.

The server MUST communicate over stdio only.

### 4.2. Tool: `start_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `text` | string | yes | MUST NOT be empty. |

**Behavior:**

1. A session ID is generated as described in Section 6.2.
2. The session directory is created with `ORIGINAL.txt` set to `text`, `CURRENT.txt` set to `text`, and `SYSTEM.txt` set to the config's `system_prompt`.
3. An agent run is performed (Section 5) with the first user message `Rewrite CURRENT.txt.`
4. On success, the result MUST be a single text content block:

   ```
   session_id: <id>

   <contents of CURRENT.txt>
   ```

5. On failure, the session directory MUST be deleted, and no session ID is returned.

### 4.3. Tool: `revise_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `session_id` | string | yes | MUST match `\A[0-9a-f]{8}\z`. |
| `feedback` | string | yes | MUST NOT be empty. |

**Behavior:**

1. `session_id` is validated against `\A[0-9a-f]{8}\z` before any filesystem access.
2. The session directory is located as described in Section 6.3. When no directory is found, a tool error MUST be returned.
3. An agent run is performed (Section 5) with the system prompt read from the session's `SYSTEM.txt` and the user message:

   ```
   Feedback on CURRENT.txt:

   <feedback>
   ```

4. On success, the result MUST be a single text content block containing only the contents of `CURRENT.txt`.

### 4.4. Errors

All failures MUST be returned as MCP tool errors (`isError: true`) with a readable message. A tool error MUST NOT terminate the server process. Failures include:

- Parameter validation failures.
- An unknown session ID.
- An unreachable platform, a non-2xx response, or a model request that exceeds the 120-second timeout.
- An agent run that exceeds `max_steps`.
- An agent run that ends with an empty `CURRENT.txt`.

### 4.5. Concurrency

Calls on the same session MUST be serialized by a per-session mutex held within the `editor mcp` process. A second call on a busy session MUST wait until the first finishes. Calls on different sessions MAY run in parallel.

## 5. Agent Run

### 5.1. Instructions

The system message for each run MUST be composed of a built-in preamble followed by the session's system prompt.

The built-in preamble is fixed in the binary and MUST state that:

- The session consists of the files `ORIGINAL.txt`, `CURRENT.txt`, and any `SCRATCH-<n>.txt` files.
- `ORIGINAL.txt` holds the original text and is read-only.
- `CURRENT.txt` holds the working rewrite, and the final rewrite MUST be left in `CURRENT.txt`.
- `SCRATCH-<n>.txt` files MAY be created for notes, and persist across revisions of the session.
- All text replies are dropped. Only the contents of `CURRENT.txt` are returned.
- The run ends when a reply is given without a tool call.

The default `system_prompt` written by `editor init` MUST be:

```
Rewrite the text for clarity and concision. Preserve the meaning, tone, and any technical details. Do not add new information.
```

`SYSTEM.txt` is written once at `start_rewrite` and MUST be used for every revision of the session, so later edits to the config's `system_prompt` do not affect existing sessions. The platform, base URL, API key, model, and `max_steps` MUST come from the config loaded at startup.

### 5.2. Conversation State

Each call MUST start a fresh agent run. No message history is stored or replayed across calls. Session state is held only in the session files.

### 5.3. Step Limit

A run MUST be limited to `max_steps` steps. A run that reaches the limit without finishing MUST fail.

### 5.4. Model Requests

Each model request MUST time out after 120 seconds. For `openai-compatible`, requests MUST include `Authorization: Bearer <api_key>` when `api_key` is non-empty.

The agent's final text reply MUST be discarded.

## 6. Sessions

### 6.1. Location

Sessions MUST be stored under `$XDG_STATE_HOME/editor/sessions/`. When `XDG_STATE_HOME` is unset or empty, `~/.local/state/editor/sessions/` MUST be used.

Each session is a directory named `<id>-<unixtime>`, where `<unixtime>` is the session creation time in Unix seconds. Session directories MUST be created with mode `0700` and session files with mode `0600`.

### 6.2. ID Generation

A session ID MUST be 8 lowercase hexadecimal characters generated from `crypto/rand`. When any entry matching `<id>-*` already exists in the sessions directory, a new ID MUST be generated.

The unixtime MUST NOT be included in any tool result. Only the ID is exposed to MCP clients.

### 6.3. Lookup

A session is located by matching `<id>-*` in the sessions directory. When no entry matches, the session is unknown.

### 6.4. Files

| File | Written by | Agent access |
| --- | --- | --- |
| `ORIGINAL.txt` | `start_rewrite` | read |
| `CURRENT.txt` | `start_rewrite`, agent | read, write |
| `SYSTEM.txt` | `start_rewrite` | none |
| `SCRATCH-<n>.txt` | agent | create, read, write |

### 6.5. Retention

Sessions MUST NOT be deleted automatically. Sessions are deleted only by `editor prune` (Section 8).

## 7. Agent Tools

### 7.1. Sandbox

Every agent tool that accepts a file name MUST check the name against the allowlist:

```
\A(ORIGINAL\.txt|CURRENT\.txt|SCRATCH-[0-9]+\.txt)\z
```

Names are matched as bare file names and MUST NOT be resolved as paths. No tool provides access outside the session directory, and no tool executes commands.

A tool failure, including a disallowed name, MUST be returned to the agent as an error tool result and MUST NOT fail the run.

### 7.2. Overlay

During a run, all writes MUST be held in an in-memory overlay. `read_file`, `grep`, and `list_files` MUST read from the overlay first and fall back to disk. Files on disk MUST NOT be modified during a run.

### 7.3. Commit

When a run succeeds, each file changed in the overlay MUST be written to `<name>.tmp` in the session directory and then renamed over `<name>`. `SCRATCH-<n>.txt` files MUST be committed before `CURRENT.txt`.

When a run fails, the overlay MUST be discarded.

### 7.4. `list_files()`

Takes no arguments. Returns the allowed files present in the session, including files created in the overlay, one per line, each followed by its size in bytes.

### 7.5. `read_file(name)`

Returns the full contents of `name`. A missing file MUST be reported as an error result.

### 7.6. `write_file(name, content)`

Replaces the full contents of `name` with `content`. A `SCRATCH-<n>.txt` file MUST be created when absent. A write to `ORIGINAL.txt` MUST be refused.

### 7.7. `edit_file(name, old_string, new_string)`

Replaces the single occurrence of `old_string` in `name` with `new_string`.

- An empty `old_string` MUST be refused.
- When `old_string` occurs zero times or more than once, the edit MUST be refused, and the error result MUST state the number of occurrences found.
- An edit to `ORIGINAL.txt` MUST be refused.

### 7.8. `grep(pattern, name?)`

Searches with a Go RE2 regular expression. When `name` is given, only that file is searched. Otherwise every allowed file in the session is searched. Matches MUST be returned one per line as `name:line:text`, with 1-based line numbers. An invalid pattern MUST be reported as an error result.

## 8. `editor prune`

### 8.1. Arguments

`--older-than <duration>` is REQUIRED. When the flag is absent, the command MUST exit with a usage error and MUST NOT delete anything.

The duration MUST accept Go `time.ParseDuration` syntax plus a `d` suffix for days, for example `30d`, `12h`, or `90m`.

### 8.2. Behavior

Session age MUST be computed from the `<unixtime>` in the directory name. Every session directory older than the duration MUST be deleted. Entries that do not match `\A[0-9a-f]{8}-[0-9]+\z` MUST be ignored.

The ID of each deleted session MUST be printed, one per line, followed by the total count. When the sessions directory does not exist, a count of 0 MUST be printed.
