# Terminal demo for the README. Render it with `make demo`.
#
# Needs: vhs (brew install vhs), a build of dir2mcp in the repo root
# (make build), and an Ollama server with nomic-embed-text and qwen2.5:7b.
# The default endpoint is http://127.0.0.1:11434/v1; set DEMO_OLLAMA_URL
# and DEMO_CHAT_MODEL to use another one. Every answer in the GIF comes
# from a real run.

Output assets/demo.gif

Set Shell bash
Set FontSize 15
Set Width 960
Set Height 560
Set Padding 18
Set Framerate 12
Set TypingSpeed 40ms
Set CursorBlink false
Set Theme { "name": "danbi", "background": "#06101E", "foreground": "#E6EDF3", "cursor": "#D0FF00", "selection": "#1D2A3F", "black": "#06101E", "red": "#FF6B6B", "green": "#D0FF00", "yellow": "#FFD166", "blue": "#7AA2F7", "magenta": "#C792EA", "cyan": "#7FDBCA", "white": "#E6EDF3", "brightBlack": "#5C6B80", "brightRed": "#FF8E8E", "brightGreen": "#E2FF66", "brightYellow": "#FFE08A", "brightBlue": "#A5C0FF", "brightMagenta": "#DDB6F2", "brightCyan": "#A8EFE0", "brightWhite": "#FFFFFF" }

Hide
Type "export BASH_SILENCE_DEPRECATION_WARNING=1; source assets/demo/setup.sh && clear"
Enter
Sleep 1s
Show

Type "ls"
Enter
Sleep 1s

Type "cat .dir2mcp.yaml"
Enter
Sleep 2.5s

Type "dir2mcp up"
Enter
Wait@90s /^\$\s*$/
Sleep 3s

# Indexing takes a few seconds. Wait off camera, then clear the screen.
Hide
Type "demo_wait && clear"
Enter
Wait@90s /^\$\s*$/
Show

Type 'dir2mcp ask "When is the budget meeting?"'
Enter
Wait+Screen@90s /Citations/
Sleep 4s

Hide
Type 'dir2mcp down; cd / && rm -rf "$DEMO_ROOT"'
Enter
Wait@30s /^\$\s*$/
