Squiggly
~~~~~~~~

Squiggly is an interactive MCP for refining Claude's prose through collaboration with another agent. The primary agent can provide feedback to the rewriting agent so that no detail is lost.

It works pretty well with Gemma 4 31B. I'm curious what you use, and what prompt works well for you (it's all configurable). Let me know in the Discussions!

(Vibe coded ✌)

Installation
≈≈≈≈≈≈≈≈≈≈≈≈

Run this to install it:

    echo "install location: $(go env GOPATH)/bin/squiggly"
    go install github.com/zachahn/squiggly@latest
    claude mcp add squiggly --scope user -- "$(go env GOPATH)/bin/squiggly" mcp

You'll need to configure Squiggly to connect to an LLM.

    squiggly init

License
≈≈≈≈≈≈≈

Copyright (c) 2026 Zach Ahn
AGPL 3.0
