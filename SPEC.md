# editor: LLM Rewriting CLI and MCP Server

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in BCP 14 (RFC2119) (RFC8174) when, and only when, they appear in all capitals, as shown here.

## 1. Overview

`editor` is a command-line program written in Go. Text is rewritten by an LLM, and the program is exposed to MCP clients as an MCP server over stdio.

Rewriting is performed by an agent loop. The agent is given a sandboxed set of file tools scoped to a single session. Session files are stored in a SQLite database. The original text is stored as `ORIGINAL.txt`, and the rewrite is produced in `CURRENT.txt`. A session is started with the original text and MAY be revised any number of times with feedback.

Every tool call received by the MCP server is recorded in the database, together with the result and the full transcript of the agent run, including model reasoning and text replies (Section 9).

The program MUST be built with:

- `github.com/mark3labs/mcp-go` for the MCP server.
- `github.com/cloudwego/eino` for the agent loop, with chat model components from `github.com/cloudwego/eino-ext`. The choice between eino agent APIs (for example, the ADK `ChatModelAgent` or `flow/agent/react`) is left to the implementation.
- `modernc.org/sqlite` for the database, through `database/sql`.

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
| `request_timeout_seconds` | integer | no | Timeout for each model request, in seconds. MUST be at least 1. Defaults to 600 when absent. |

### 2.3. Base URL

- For `ollama`, `base_url` is the server root, for example `http://localhost:11434`.
- For `llamacpp` and `openai-compatible`, `base_url` is the OpenAI-style API root including the version segment, for example `http://localhost:9931/v1` or `https://api.openai.com/v1`.

### 2.4. Permissions

The config directory MUST be created with mode `0700` when absent. The config file MUST be written with mode `0600`, because the API key is stored in plaintext.

### 2.5. Example

```toml
platform = "openai-compatible"
base_url = "https://openrouter.ai/api/v1"
api_key = "sk-or-..."
model = "anthropic/claude-sonnet-5.5"
max_steps = 25
request_timeout_seconds = 600
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
3. When the user confirms, each subsequent prompt MUST be pre-filled with the value from the existing file. `system_prompt`, `max_steps`, and `request_timeout_seconds` MUST be preserved from the existing file without prompting.

When the existing file cannot be parsed, the user MUST still be asked to confirm overwriting it, and prompts MUST fall back to the defaults in Section 3.3.

### 3.3. Prompts

Prompts MUST appear in this order:

1. **Overwrite confirmation.** Shown only when a config file exists.
2. **Platform.** One of `ollama`, `llamacpp`, `openai-compatible`.
3. **Base URL.** The default is `http://localhost:11434` for `ollama` and `http://localhost:9931/v1` for `llamacpp`. No default is offered for `openai-compatible`, and a value MUST be entered.
4. **API key.** Shown only for `openai-compatible`. MAY be left blank.
5. **Model.** See Section 3.4.

### 3.4. Model Discovery

Before the model prompt, the available models MUST be requested from the platform:

- For `ollama`: `GET <base_url>/api/tags`.
- For `llamacpp` and `openai-compatible`: `GET <base_url>/models`. When an API key was entered, the request MUST include `Authorization: Bearer <api_key>`.

When one or more models are returned, a numbered pick list MUST be shown and a model MUST be selected by number. When exactly one model is returned, that model MUST be offered as the default.

When the request fails or returns no models, the reason MUST be printed and the user MUST be asked to type a model name. An empty model name MUST be rejected and the prompt repeated.

### 3.5. Writing

After all prompts are answered, the config file MUST be written as described in Section 2. For a new config, `system_prompt` MUST be set to the default in Section 5.1, `max_steps` MUST be set to 25, and `request_timeout_seconds` MUST be set to 600. The path of the written file MUST be printed.

## 4. `editor mcp`

### 4.1. Startup

The config MUST be read once at startup and MUST NOT be read again during the life of the process. Edits to the config file take effect only after the server is restarted.

When the config file is missing or invalid (unparseable, unknown `platform`, empty `base_url`, empty `model`, empty `system_prompt`, `max_steps` below 1, or `request_timeout_seconds` below 1), the process MUST exit with a non-zero status and an error message on stderr that suggests running `editor init`.

After the config is loaded, the database MUST be opened and migrated as described in Section 6.2. When the database cannot be opened or migrated, the process MUST exit with a non-zero status and an error message on stderr.

The server MUST communicate over stdio only.

### 4.2. Tool: `start_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `text` | string | yes | MUST NOT be empty. |

**Behavior:**

1. A request row is recorded with status `running` (Section 9.2).
2. An agent run is performed (Section 5) with the config's `system_prompt` and the first user message `Rewrite CURRENT.txt.` The overlay starts with `ORIGINAL.txt` and `CURRENT.txt` both set to `text`. No session row exists during the run.
3. On success, the session is created and the overlay is committed in one transaction (Section 7.3). The session ID is generated as described in Section 6.3.
4. On success, the result MUST be a single text content block:

   ```
   session_id: <id>

   <contents of CURRENT.txt>
   ```

5. On failure, no session is created and no session ID is returned. The request row and transcript MUST still be recorded, with a NULL `session_id`.

### 4.3. Tool: `revise_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `session_id` | string | yes | MUST match `\A[0-9a-f]{8}\z`. |
| `feedback` | string | yes | MUST NOT be empty. |

**Behavior:**

1. `session_id` is validated against `\A[0-9a-f]{8}\z` before the session is looked up.
2. The session is located as described in Section 6.4. When no session is found, a tool error MUST be returned.
3. A request row is recorded with status `running` (Section 9.2).
4. An agent run is performed (Section 5) with the session's `system_prompt` and the user message:

   ```
   Feedback on CURRENT.txt:

   <feedback>
   ```

5. On success, the overlay is committed (Section 7.3), and the result MUST be a single text content block containing only the contents of `CURRENT.txt`.

### 4.4. Errors

All failures MUST be returned as MCP tool errors (`isError: true`) with a readable message. A tool error MUST NOT terminate the server process. Failures include:

- Parameter validation failures.
- An unknown session ID.
- An unreachable platform, a non-2xx response, or a model request that exceeds `request_timeout_seconds`.
- An agent run that exceeds `max_steps`.
- An agent run that ends with an empty `CURRENT.txt`.
- A version conflict at commit (Section 4.5).
- A database read or write failure.

### 4.5. Concurrency

Calls on the same session MUST be serialized by a per-session mutex held within the `editor mcp` process. A second call on a busy session MUST wait until the first finishes. Calls on different sessions MAY run in parallel.

Several `editor mcp` processes MAY share the database. Calls on the same session from different processes are not serialized. Instead, the session `version` read at the start of a run MUST be compared at commit (Section 7.3). When the version has changed, the commit MUST be refused, nothing is written to `files`, and a tool error MUST be returned stating that the session was revised by another process during the run.

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
Rewrite the text for clarity and concision. Preserve the meaning, tone, and any technical details. Do not add new information. Do not imagine a scenario; restate factually.

- Use American English.
- Avoid anthropomorphizing inanimate/software objects.
- Avoid using the present tense verb "names" and "carries" as it's ambiguous and anthropomorphizing.
- Avoid the phrase "$verb it".

<example>
<summary>Avoid the phrase "$verb it". Avoid anthropomorphizing inanimate/software objects.</summary>
<comparison>
- BAD: ... before the bank settled it
- GOOD: ... before it is settled by the bank
</comparison>
</example>
```

The session's `system_prompt` is stored once when the session is created and MUST be used for every revision of the session, so later edits to the config's `system_prompt` do not affect existing sessions. The platform, base URL, API key, model, `max_steps`, and `request_timeout_seconds` MUST come from the config loaded at startup.

### 5.2. Conversation State

Each call MUST start a fresh agent run. Transcripts are recorded for inspection (Section 9) and MUST NOT be replayed into later runs. Session state is held only in the session files.

### 5.3. Step Limit

A run MUST be limited to `max_steps` steps. A run that reaches the limit without finishing MUST fail.

### 5.4. Model Requests

Each model request MUST time out after `request_timeout_seconds` seconds. For `openai-compatible`, requests MUST include `Authorization: Bearer <api_key>` when `api_key` is non-empty.

The agent's final text reply MUST NOT be returned to the MCP client. The reply is recorded in the transcript (Section 9.3).

## 6. Sessions

### 6.1. Database Location

All state MUST be stored in a single SQLite database at `$XDG_STATE_HOME/editor/editor.db`. When `XDG_STATE_HOME` is unset or empty, `~/.local/state/editor/editor.db` MUST be used.

The directory MUST be created with mode `0700` when absent. The database file MUST have mode `0600`, because session text is stored in it.

Each connection MUST enable `journal_mode=WAL`, `foreign_keys=ON`, and a `busy_timeout` of at least 5000 milliseconds.

All timestamps MUST be stored as integer Unix milliseconds.

### 6.2. Schema

The schema version MUST be tracked with `PRAGMA user_version`. On open, each migration above the stored version MUST be applied in order, each in its own transaction that also sets `user_version`.

```sql
CREATE TABLE sessions (
  id            TEXT PRIMARY KEY,
  created_at    INTEGER NOT NULL,
  system_prompt TEXT NOT NULL,
  version       INTEGER NOT NULL
);

CREATE TABLE files (
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL,
  PRIMARY KEY (session_id, name)
);

CREATE TABLE requests (
  id          INTEGER PRIMARY KEY,
  session_id  TEXT REFERENCES sessions(id) ON DELETE CASCADE,
  tool        TEXT NOT NULL,
  arguments   TEXT NOT NULL,
  before      TEXT,
  output      TEXT,
  status      TEXT NOT NULL,
  error       TEXT,
  platform    TEXT NOT NULL,
  model       TEXT NOT NULL,
  steps       INTEGER,
  started_at  INTEGER NOT NULL,
  duration_ms INTEGER
);

CREATE TABLE messages (
  request_id   INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,
  role         TEXT NOT NULL,
  content      TEXT NOT NULL,
  reasoning    TEXT NOT NULL,
  tool_calls   TEXT,
  tool_call_id TEXT,
  tool_name    TEXT,
  PRIMARY KEY (request_id, seq)
);

CREATE INDEX requests_session_id ON requests(session_id);
CREATE INDEX requests_started_at ON requests(started_at);
```

### 6.3. ID Generation

A session ID MUST be 8 lowercase hexadecimal characters generated from `crypto/rand`. When the insert into `sessions` fails on the primary key, a new ID MUST be generated and the insert retried within the same transaction.

Only the ID is exposed to MCP clients.

### 6.4. Lookup

A session is located by its `id` in `sessions`. When no row matches, the session is unknown.

### 6.5. Files

| File | Written by | Agent access |
| --- | --- | --- |
| `ORIGINAL.txt` | `start_rewrite` | read |
| `CURRENT.txt` | `start_rewrite`, agent | read, write |
| `SCRATCH-<n>.txt` | agent | create, read, write |

Files are stored as rows in `files`. The session's system prompt is stored in `sessions.system_prompt` and is not accessible to the agent.

### 6.6. Retention

Sessions MUST NOT be deleted automatically. Sessions are deleted only by `editor prune` (Section 8).

## 7. Agent Tools

### 7.1. Sandbox

Every agent tool that accepts a file name MUST check the name against the allowlist:

```
\A(ORIGINAL\.txt|CURRENT\.txt|SCRATCH-[0-9]+\.txt)\z
```

Names are matched as bare file names and MUST NOT be resolved as paths. No tool provides access outside the session's files, and no tool executes commands.

A tool failure, including a disallowed name, MUST be returned to the agent as an error tool result and MUST NOT fail the run.

### 7.2. Overlay

At the start of a run, the session's files and `version` MUST be read in one transaction and held in memory as the base snapshot. No database transaction is held open during the run.

During a run, all writes MUST be held in an in-memory overlay. `read_file`, `grep`, and `list_files` MUST read from the overlay first and fall back to the base snapshot. The database MUST NOT be modified by agent tools during a run.

### 7.3. Commit

When a run succeeds, one transaction MUST:

1. For `start_rewrite`, insert the session row with `version` 1, and insert `ORIGINAL.txt`.
2. For `revise_rewrite`, increment `version` with `UPDATE sessions SET version = version + 1 WHERE id = ? AND version = ?`. When no row is updated, the transaction MUST be rolled back and a version conflict reported (Section 4.5).
3. Upsert each file changed in the overlay into `files`.
4. Complete the request row and insert the transcript (Section 9).

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

When the database file does not exist, a count of 0 MUST be printed and the database MUST NOT be created. Otherwise, the database is opened and migrated as described in Section 6.2.

Session age MUST be computed from `sessions.created_at`. In one transaction:

1. Every session older than the duration MUST be deleted. Its files, requests, and messages are deleted by cascade.
2. Every request with a NULL `session_id` and a `started_at` older than the duration MUST be deleted, together with its messages.

The ID of each deleted session MUST be printed, one per line, followed by the total count of deleted sessions.

## 9. Request Log

### 9.1. Scope

Every `start_rewrite` and `revise_rewrite` call MUST be recorded in `requests`, including failed calls.

A call rejected before an agent run, because of a parameter validation failure or an unknown session ID, MUST be recorded as one completed row with status `error`, a NULL `session_id`, a NULL `steps`, and no transcript.

### 9.2. Request Rows

The row for a call that reaches an agent run MUST be inserted with status `running` before the run, in its own transaction, and completed after the run.

| Column | Value |
| --- | --- |
| `session_id` | The session ID for `revise_rewrite` on an existing session. For `start_rewrite`, set to the new ID in the commit transaction. NULL otherwise. |
| `tool` | `start_rewrite` or `revise_rewrite`. |
| `arguments` | The tool arguments as received, encoded as JSON. |
| `before` | For `revise_rewrite`, `CURRENT.txt` from the base snapshot. NULL for `start_rewrite`. |
| `output` | `CURRENT.txt` on success. NULL otherwise. |
| `status` | `running`, `ok`, or `error`. |
| `error` | The tool error message when `status` is `error`. NULL otherwise. |
| `platform`, `model` | From the config loaded at startup. |
| `steps` | The number of model responses in the run. |
| `started_at` | When the call was received. |
| `duration_ms` | Milliseconds from `started_at` to completion. NULL while `running`. |

On success, the row MUST be completed in the commit transaction (Section 7.3). On failure, the row MUST be completed and the transcript inserted in one transaction. When recording a failure itself fails, the tool error MUST include both the original error and the recording error.

A row left in `running` indicates that the process exited before the call completed. Such rows MUST NOT be modified by other processes.

### 9.3. Transcript

Every message of the agent run MUST be recorded in `messages` in order, with `seq` starting at 0. The transcript includes the system message, the user message, every assistant message, and every tool result. Messages produced before a failure, such as a run that exceeds `max_steps` or a failed model request, MUST be recorded.

| Column | Value |
| --- | --- |
| `role` | `system`, `user`, `assistant`, or `tool`. |
| `content` | The message text. An empty string when there is no text. |
| `reasoning` | The model reasoning returned by the platform. An empty string when none is returned. |
| `tool_calls` | For assistant messages with tool calls, a JSON array of objects with `id`, `name`, and `arguments`. NULL otherwise. |
| `tool_call_id`, `tool_name` | For tool results, the ID and name of the tool call being answered. NULL otherwise. |

Reasoning is available only when returned by the platform, for example `reasoning_content` from `llama-server`, `reasoning` from OpenRouter, or `thinking` from Ollama.

### 9.4. Inspection

No command is provided for reading the log. The database is read with the `sqlite3` CLI, for example:

```sql
SELECT id, session_id, tool, status, steps, duration_ms, error
FROM requests ORDER BY started_at DESC LIMIT 20;

SELECT seq, role, reasoning, content, tool_calls
FROM messages WHERE request_id = ? ORDER BY seq;
```
