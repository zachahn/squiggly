package prose

const Usage = `Usage:
  squiggly init                            Interactively create or update the config file.
  squiggly mcp                             Run the MCP server over stdio.
  squiggly prune --older-than <duration>   Delete sessions inactive for longer than the specified duration.
`

const (
	PruneUsage               = "Usage: squiggly prune --older-than <duration>\n"
	OlderThanFlagDescription = "sessions inactive for longer than this duration are deleted (e.g., 30d, 12h, 90m)"
)

const ServerInstructions = "Rewrite prose using Squiggly through a back-and-forth with a rewriting agent. A single `start_rewrite` call produces an initial draft, not a final result. Compare each draft to your original text and your intent for missing details, altered meaning, incorrect tone, or unwanted additions. Use `revise_rewrite` with specific feedback and repeat until the draft is satisfactory. Accept a draft without revision only if no issues remain."

const ReviewReminder = "Compare this draft with the original text and call revise_rewrite with specific feedback until the draft is satisfactory."

const (
	StartRewriteDescription     = "Start a rewriting session. The text is rewritten by an LLM agent into an initial draft, not a final result. The result begins with a session_id line and a review reminder line, followed by a blank line and the draft. Review the draft against the original text, and pass the session_id to revise_rewrite with feedback until the draft is right. When the run fails, the error message also begins with a session_id line, and the session may still be revised."
	StartRewriteTextDescription = "Text to rewrite."

	ReviseRewriteDescription          = "Revise the rewrite of an existing session with feedback. The result contains only the revised text."
	ReviseRewriteSessionIDDescription = "Session ID returned by start_rewrite."
	ReviseRewriteFeedbackDescription  = "Feedback on the current rewrite."
)

const (
	FirstRunMessage = "Rewrite CURRENT.txt."
	FeedbackPrefix  = "Feedback on CURRENT.txt:\n\n"
)

const AgentPreamble = `You rewrite text in a sandboxed session consisting of ORIGINAL.txt, CURRENT.txt, and any SCRATCH-<n>.txt files (e.g., SCRATCH-1.txt).

- ORIGINAL.txt: Read-only; contains the original text.
- CURRENT.txt: Contains the working rewrite; the final rewrite MUST be left in CURRENT.txt.
- SCRATCH-<n>.txt: Files that may be created for notes; they persist across session revisions.
- All text replies are discarded; only the contents of CURRENT.txt are returned.
- The run ends when a reply is provided without a tool call.

Use list_files, read_file, write_file, edit_file, and grep to manage these files. Read CURRENT.txt before making changes.

The rewriting instructions follow.

`

const DefaultSystemPrompt = `Rewrite the text for clarity and concision. Preserve the meaning, tone, and technical details. Do not add new information or imagine scenarios; restate factually.

- Use American English.
- Avoid anthropomorphizing inanimate or software objects.
- Avoid the present tense verbs "names" and "carries", as they are ambiguous and anthropomorphic.
- Avoid the phrase "$verb it".

<example>
<summary>Avoid the phrase "$verb it" and anthropomorphizing inanimate or software objects.</summary>
<comparison>
- BAD: ... before the bank settled it
- GOOD: ... before it is settled by the bank
</comparison>
</example>
`

const (
	FileNameParamDescription = "File name: ORIGINAL.txt, CURRENT.txt, or SCRATCH-<n>.txt."

	ListFilesDescription = "List session files and their sizes in bytes, one per line."

	ReadFileDescription = "Return the full contents of a session file."

	WriteFileDescription        = "Replace the full contents of a session file. SCRATCH-<n>.txt files are created if absent. Read-only files, such as ORIGINAL.txt, cannot be written."
	WriteFileContentDescription = "New full contents of the file."

	EditFileDescription          = "Replace a single occurrence of old_string in a session file with new_string. The edit fails if old_string is empty or does not occur exactly once. Read-only files, such as ORIGINAL.txt, cannot be edited."
	EditFileOldStringDescription = "Exact text to replace; must occur exactly once."
	EditFileNewStringDescription = "Replacement text."

	GrepDescription        = "Search session files using a Go RE2 regular expression. Return matches one per line as name:line:text with 1-based line numbers. Search all session files when name is omitted."
	GrepPatternDescription = "Go RE2 regular expression."
	GrepNameDescription    = "Optional file name to restrict the search."
)
