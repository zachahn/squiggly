# editor: LLM Rewriting CLI and MCP Server

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in BCP 14 (RFC2119) (RFC8174) when, and only when, they appear in all capitals, as shown here.

## 1. Overview

`editor` is a command-line program written in Go. Text is rewritten by an LLM, and the program is exposed to MCP clients as an MCP server over stdio.

Rewriting is performed by an agent loop. The agent is given a sandboxed set of file tools scoped to a single session. Sessions and session files are stored in a SQLite database (Section 6). The original text is stored as `ORIGINAL.txt`, and the rewrite is produced in `CURRENT.txt`. A session is started with the original text and MAY be revised any number of times with feedback.

Every MCP tool call is recorded in the database, together with the full transcript of the agent run, for debugging (Section 9).

The program MUST be built with:

- `github.com/mark3labs/mcp-go` for the MCP server.
- `github.com/cloudwego/eino` for the agent loop, with chat model components from `github.com/cloudwego/eino-ext`. The choice between eino agent APIs (for example, the ADK `ChatModelAgent` or `flow/agent/react`) is left to the implementation.
- `modernc.org/sqlite` for the database, through `database/sql`.

### 1.1. Commands

| Command | Purpose |
| --- | --- |
| `editor init` | Interactively create or update the config file. |
| `editor mcp` | Run the MCP server over stdio. |
| `editor prune --older-than <duration>` | Delete sessions inactive for longer than a given age, together with their request logs. |

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

After the config is loaded, the database MUST be opened and migrated as described in Section 6. When the database cannot be opened or migrated, the process MUST exit with a non-zero status and an error message on stderr.

The server MUST communicate over stdio only.

### 4.2. Tool: `start_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `text` | string | yes | MUST NOT be empty. |

**Behavior:**

1. Parameters are validated. A call that fails validation is rejected and recorded as described in Section 9.2.
2. In one transaction:
   - A session row is inserted with a new public ID (Section 6.3), `system_prompt` set to the config's `system_prompt`, and `version` set to 0.
   - `ORIGINAL.txt` is inserted with `writable` set to 0, and `CURRENT.txt` is inserted with `writable` set to 1. Each file is given one version with content `text` and a NULL `request_id`.
   - The request row is inserted with status `running` (Section 9.3).
3. An agent run is performed (Section 5) with the first user message `Rewrite CURRENT.txt.`
4. On success, the run is committed (Section 7.3), and the result MUST be a single text content block:

   ```
   session_id: <public_id>

   <contents of CURRENT.txt>
   ```

5. On failure, the session MUST be kept, and the tool error message MUST be:

   ```
   session_id: <public_id>

   <error message>
   ```

   The session MAY then be revised with `revise_rewrite`.

### 4.3. Tool: `revise_rewrite`

**Parameters:**

| Name | Type | Required | Constraint |
| --- | --- | --- | --- |
| `session_id` | string | yes | MUST match `\A[0-9a-f]{8}\z`. |
| `feedback` | string | yes | MUST NOT be empty. |

**Behavior:**

1. Parameters are validated before the session is looked up. A call that fails validation is rejected and recorded as described in Section 9.2.
2. The session is located as described in Section 6.4. When no session is found, the call is rejected and recorded as described in Section 9.2.
3. The per-session mutex is acquired (Section 4.5).
4. The request row is inserted with status `running` (Section 9.3).
5. An agent run is performed (Section 5) with the session's `system_prompt` and the user message:

   ```
   Feedback on CURRENT.txt:

   <feedback>
   ```

6. On success, the run is committed (Section 7.3), and the result MUST be a single text content block containing only the contents of `CURRENT.txt`.

### 4.4. Errors

All failures MUST be returned as MCP tool errors (`isError: true`) with a readable message. A tool error MUST NOT terminate the server process. Failures include:

- Parameter validation failures.
- An unknown session ID.
- An unreachable platform, a non-2xx response, or a model request that exceeds `request_timeout_seconds`.
- An agent run that exceeds `max_steps`.
- An agent run that reaches its `deadline_at` (Section 5.3).
- An agent run that ends with an empty `CURRENT.txt`.
- A version conflict at commit (Section 4.5).
- A database read or write failure.

### 4.5. Concurrency

Calls on the same session MUST be serialized by a per-session mutex, keyed by public ID and held within the `editor mcp` process. A second call on a busy session MUST wait until the first finishes. Calls on different sessions MAY run in parallel.

Several `editor mcp` processes MAY share the database. Calls on the same session from different processes are not serialized. Instead, the session `version` read at the start of a run MUST be compared at commit (Section 7.3). When the version has changed, the commit MUST be refused, and a tool error MUST be returned stating that the session was revised by another process during the run.

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

The session's system prompt is stored in `sessions.system_prompt` when the session is created and MUST be used for every revision of the session, so later edits to the config's `system_prompt` do not affect existing sessions. The platform, base URL, API key, model, `max_steps`, and `request_timeout_seconds` MUST come from the config loaded at startup.

### 5.2. Conversation State

Each call MUST start a fresh agent run. Transcripts are recorded for debugging (Section 9.4) and MUST NOT be replayed into later runs. Session state is held only in the session files.

### 5.3. Limits

A run MUST be limited to `max_steps` steps. A run that reaches the limit without finishing MUST fail.

A run MUST also fail when the request's `deadline_at` (Section 9.3) is reached before the run finishes.

### 5.4. Model Requests

Each model request MUST time out after `request_timeout_seconds` seconds. For `openai-compatible`, requests MUST include `Authorization: Bearer <api_key>` when `api_key` is non-empty.

The agent's final text reply MUST NOT be returned to the MCP client. The reply is recorded in the transcript (Section 9.4).

## 6. Database and Sessions

### 6.1. Location

All state MUST be stored in a single SQLite database at `$XDG_STATE_HOME/editor/editor.db`. When `XDG_STATE_HOME` is unset or empty, `~/.local/state/editor/editor.db` MUST be used.

The directory MUST be created with mode `0700` when absent. The database file MUST have mode `0600`, because session text is stored in the database.

Each connection MUST enable `journal_mode=WAL`, `foreign_keys=ON`, and a `busy_timeout` of at least 5000 milliseconds.

All timestamps MUST be stored as integer Unix milliseconds. Booleans MUST be stored as integer 0 or 1.

Session directories from earlier versions, under `$XDG_STATE_HOME/editor/sessions/`, MUST be ignored. They are neither read nor deleted.

### 6.2. Schema

The schema version MUST be tracked with `PRAGMA user_version`. On open, each migration above the stored version MUST be applied in order, each in its own transaction that also sets `user_version`.

```sql
CREATE TABLE sessions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id     TEXT NOT NULL UNIQUE,
  system_prompt TEXT NOT NULL,
  version       INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  writable   INTEGER NOT NULL CHECK (writable IN (0, 1)),
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, name)
);

CREATE TABLE requests (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
  tool        TEXT NOT NULL,
  arguments   TEXT NOT NULL,
  status      TEXT NOT NULL,
  error       TEXT,
  platform    TEXT NOT NULL,
  model       TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  deadline_at INTEGER NOT NULL,
  finished_at INTEGER
);

CREATE TABLE file_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  request_id INTEGER REFERENCES requests(id) ON DELETE CASCADE,
  content    TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  role              TEXT NOT NULL,
  content           TEXT NOT NULL,
  reasoning         TEXT NOT NULL,
  tool_calls        TEXT,
  tool_call_id      TEXT,
  tool_name         TEXT,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  total_tokens      INTEGER,
  created_at        INTEGER NOT NULL
);

CREATE INDEX requests_session_id ON requests(session_id);
CREATE INDEX requests_started_at ON requests(started_at);
CREATE INDEX file_versions_file_id ON file_versions(file_id);
CREATE INDEX file_versions_request_id ON file_versions(request_id);
CREATE INDEX messages_request_id ON messages(request_id);
```

### 6.3. IDs

Every table uses an integer `id` primary key for internal references. An `id` value MUST NOT be included in any tool result and MUST NOT be accepted as a tool argument.

A session's public ID MUST be 8 lowercase hexadecimal characters generated from `crypto/rand`. When the insert into `sessions` fails on the `public_id` unique constraint, a new public ID MUST be generated and the insert retried within the same transaction. Only the public ID is exposed to MCP clients.

### 6.4. Lookup

A session is located by `sessions.public_id`. When no row matches, the session is unknown.

### 6.5. Files

| File | `writable` | Created by | Agent access |
| --- | --- | --- | --- |
| `ORIGINAL.txt` | 0 | `start_rewrite` | read |
| `CURRENT.txt` | 1 | `start_rewrite` | read, write |
| `SCRATCH-<n>.txt` | 1 | agent | create, read, write |

The session's system prompt is stored in `sessions.system_prompt` and is not accessible to the agent.

### 6.6. File Versions

File contents are stored in `file_versions`. Each change to a file MUST be stored as a new row. Rows in `file_versions` MUST NOT be updated.

The current version of a file is the row with the highest `id` among the file's versions whose `request_id` is NULL or references a request with status `ok`. Versions written by failed requests are kept for debugging and never become current. A file with no current version is treated as absent from the session.

The history of a file is the file's versions in `id` order. The request that produced each version is identified by `request_id`.

### 6.7. Retention

Sessions MUST NOT be deleted automatically. Sessions are deleted only by `editor prune` (Section 8).

## 7. Agent Tools

### 7.1. Sandbox

Every agent tool that accepts a file name MUST check the name against the allowlist:

```
\A(ORIGINAL\.txt|CURRENT\.txt|SCRATCH-[0-9]+\.txt)\z
```

Names are matched as bare file names and MUST NOT be resolved as paths. No tool provides access outside the session's files, and no tool executes commands.

A write or edit to a file whose `writable` is 0 MUST be refused. A file MAY be created by the agent only when the name matches `SCRATCH-[0-9]+\.txt`. Files created by the agent have `writable` set to 1.

A tool failure, including a disallowed name, MUST be returned to the agent as an error tool result and MUST NOT fail the run.

### 7.2. Overlay

At the start of a run, the session's `version`, the `writable` flag of each file, and the current version of each file (Section 6.6) MUST be read in one transaction and held in memory as the base snapshot. No database transaction is held open during the run.

During a run, all writes MUST be held in an in-memory overlay. `read_file`, `grep`, and `list_files` MUST read from the overlay first and fall back to the base snapshot. The database MUST NOT be modified by agent tools during a run.

### 7.3. Commit

When a run succeeds, one transaction MUST:

1. Increment the session version with `UPDATE sessions SET version = version + 1 WHERE id = ? AND version = ?`, using the version from the base snapshot. When no row is updated, the transaction MUST be rolled back, and the run MUST be handled as a failure with a version conflict (Section 4.5).
2. For each file written in the overlay, insert a `files` row when none exists for the name, and insert a `file_versions` row referencing the request.
3. Set the request's status to `ok` and record `finished_at`.

When a run fails, including on a version conflict, one transaction MUST:

1. For each file written in the overlay, insert a `files` row when none exists for the name, and insert a `file_versions` row referencing the request.
2. Set the request's status to `error`, and record `error` and `finished_at`.

When recording a failure itself fails, the tool error MUST include both the original error and the recording error.

### 7.4. `list_files()`

Takes no arguments. Returns the files present in the session, including files created in the overlay, one per line, each followed by its size in bytes.

### 7.5. `read_file(name)`

Returns the full contents of `name`. A missing file MUST be reported as an error result.

### 7.6. `write_file(name, content)`

Replaces the full contents of `name` with `content`. A `SCRATCH-<n>.txt` file MUST be created when absent. A write to a file whose `writable` is 0 MUST be refused.

### 7.7. `edit_file(name, old_string, new_string)`

Replaces the single occurrence of `old_string` in `name` with `new_string`.

- An empty `old_string` MUST be refused.
- When `old_string` occurs zero times or more than once, the edit MUST be refused, and the error result MUST state the number of occurrences found.
- An edit to a file whose `writable` is 0 MUST be refused.

### 7.8. `grep(pattern, name?)`

Searches with a Go RE2 regular expression. When `name` is given, only that file is searched. Otherwise every file in the session is searched. Matches MUST be returned one per line as `name:line:text`, with 1-based line numbers. An invalid pattern MUST be reported as an error result.

## 8. `editor prune`

### 8.1. Arguments

`--older-than <duration>` is REQUIRED. When the flag is absent, the command MUST exit with a usage error and MUST NOT delete anything.

The duration MUST accept Go `time.ParseDuration` syntax plus a `d` suffix for days, for example `30d`, `12h`, or `90m`.

### 8.2. Behavior

When the database file does not exist, a count of 0 MUST be printed and the database MUST NOT be created. Otherwise, the database is opened and migrated as described in Section 6.2.

The last activity of a session is the latest `started_at` among the session's requests, or `sessions.created_at` when the session has no requests. In one transaction:

1. Every session whose last activity is older than the duration MUST be deleted. The session's files, file versions, requests, and messages are deleted by cascade.
2. Every request with a NULL `session_id` and a `started_at` older than the duration MUST be deleted, together with the request's messages.

The public ID of each deleted session MUST be printed, one per line, followed by the total count of deleted sessions.

## 9. Request Log

### 9.1. Scope

Every `start_rewrite` and `revise_rewrite` call MUST be recorded in `requests`, including failed and rejected calls.

### 9.2. Rejected Calls

A call rejected before an agent run, because of a parameter validation failure or an unknown session ID, MUST be recorded as one completed row with status `error`, a NULL `session_id`, `deadline_at` and `finished_at` equal to `started_at`, and no messages. The session ID sent by the client, if any, is kept in `arguments`.

### 9.3. Request Rows

The row for a call that reaches an agent run MUST be inserted with status `running` before the run, and completed as described in Section 7.3. For `start_rewrite`, the row is inserted in the transaction that creates the session. For `revise_rewrite`, the row is inserted after the per-session mutex is acquired.

| Column | Value |
| --- | --- |
| `session_id` | The session's internal ID. NULL for rejected calls. |
| `tool` | `start_rewrite` or `revise_rewrite`. |
| `arguments` | The tool arguments as received, encoded as JSON. |
| `status` | `running`, `ok`, or `error`. |
| `error` | The tool error message when `status` is `error`. NULL otherwise. |
| `platform`, `model` | From the config loaded at startup. |
| `started_at` | When the row is inserted. |
| `deadline_at` | `started_at` plus `max_steps` × `request_timeout_seconds`, in milliseconds. Equal to `started_at` for rejected calls. |
| `finished_at` | When the row is completed. NULL while `running`. |

A row MUST be modified only by the process that inserted it. A row in `running` whose `deadline_at` has passed indicates that the process exited before the call completed.

### 9.4. Transcript

Every message of the agent run MUST be inserted into `messages` as the message is produced, so a run that fails or a process that exits leaves the transcript up to that point. The transcript includes the system message, the user message, every assistant message, and every tool result, ordered by `id`.

Message fields MUST be stored as given by eino, without normalization.

| Column | Value |
| --- | --- |
| `role` | `system`, `user`, `assistant`, or `tool`. |
| `content` | The message text. An empty string when the message has no text. |
| `reasoning` | The model reasoning returned by the platform. An empty string when none is returned. |
| `tool_calls` | For assistant messages with tool calls, a JSON array of objects with `id`, `name`, and `arguments`. NULL otherwise. |
| `tool_call_id`, `tool_name` | For tool results, the ID and name of the tool call being answered. NULL otherwise. |
| `prompt_tokens`, `completion_tokens`, `total_tokens` | For assistant messages, the token usage reported by the platform. NULL when no usage is reported, and for other roles. |
| `created_at` | When the message is inserted. |

Reasoning is available only when returned by the platform, for example `reasoning_content` from `llama-server`, `reasoning` from OpenRouter, or `thinking` from Ollama.

### 9.5. Inspection

No command is provided for reading the log. The database is read with the `sqlite3` CLI, for example:

```sql
SELECT r.id, s.public_id, r.tool, r.status, r.error,
       r.finished_at - r.started_at AS duration_ms
FROM requests r LEFT JOIN sessions s ON s.id = r.session_id
ORDER BY r.started_at DESC LIMIT 20;

SELECT role, reasoning, content, tool_calls, created_at
FROM messages WHERE request_id = ? ORDER BY id;

SELECT v.id, r.tool, r.status, r.arguments, v.content
FROM file_versions v
JOIN files f ON f.id = v.file_id
JOIN sessions s ON s.id = f.session_id
LEFT JOIN requests r ON r.id = v.request_id
WHERE s.public_id = ? AND f.name = 'CURRENT.txt'
ORDER BY v.id;
```
