package main

import (
	"fmt"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/agents"
)

// cmdCompletion prints shell completion scripts for the given shell to stdout.
func cmdCompletion(shell string) int {
	names := agents.ListSorted()
	agentList := strings.Join(names, " ")

	switch shell {
	case "bash":
		fmt.Printf(`_msc() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local prev="${COMP_WORDS[COMP_CWORD-1]}"

    if [[ "$cur" == -* ]]; then
        COMPREPLY=($(compgen -W "-h --help -v --version -d --debug -q --quiet -n --dry-run -j --json -f --force --no-inject --no-auto-calibrate --inject-budget --inject-min-score --recall-mode --log-json --vault --mcp-url --token" -- "$cur"))
        return
    fi

    # Flags that take a value: don't offer commands/agents after them.
    if [[ "$prev" == "--vault" || "$prev" == "--mcp-url" || "$prev" == "--token" || "$prev" == "--inject-budget" || "$prev" == "--inject-min-score" || "$prev" == "--recall-mode" ]]; then
        return
    fi

    # Complete shell names for the completion subcommand.
    if [[ "$prev" == "completion" ]]; then
        COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
        return
    fi

    # Only complete agent names and subcommands for the first positional arg.
    local commands="%s list status version help completion"
    local subcmd=""
    local skip_next=0
    for word in "${COMP_WORDS[@]:1}"; do
        if [[ "$word" == "$cur" ]]; then
            continue
        fi
        if [[ "$skip_next" -eq 1 ]]; then
            skip_next=0
            continue
        fi
        if [[ "$word" == "--vault" || "$word" == "--mcp-url" || "$word" == "--token" || "$word" == "--inject-budget" || "$word" == "--inject-min-score" || "$word" == "--recall-mode" ]]; then
            skip_next=1
            continue
        fi
        if [[ "$word" != -* ]]; then
            subcmd="$word"
            break
        fi
    done
    if [[ -z "$subcmd" ]]; then
        COMPREPLY=($(compgen -W "$commands" -- "$cur"))
    fi
}
complete -F _msc msc
`, agentList)

	case "zsh":
		fmt.Printf(`#compdef msc

_msc() {
    local -a agents=(%s)
    local -a commands=(list status version help completion)
    local -a flags=(
        {-h,--help}'[Show help]'
        {-v,--version}'[Show version]'
        {-d,--debug}'[Enable debug logging]'
        {-q,--quiet}'[Suppress msc output]'
        {-n,--dry-run}'[Show resolved config without launching]'
        {-j,--json}'[Output as JSON]'
        {-f,--force}'[Launch even if MuninnDB is unreachable (captures lost)]'
        '--no-inject[Disable memory injection]'
        '--no-auto-calibrate[Keep the injection threshold fixed]'
        '--inject-budget[Max tokens to inject per request]:budget:'
        '--inject-min-score[Min cosine to inject a memory (0-1)]:score:'
        '--recall-mode[MuninnDB recall mode]:mode:(semantic recent balanced deep)'
        '--log-json[Emit logs as JSON]'
        '--vault[MuninnDB vault name]:vault:'
        '--mcp-url[MuninnDB MCP endpoint]:url:'
        '--token[MuninnDB bearer token]:token:'
    )

    _arguments -s \
        $flags \
        "1:command:($agents $commands)" \
        '*::arg:->args'

    case "$state" in
        args)
            case "${words[1]}" in
                completion)
                    _values 'shell' bash zsh fish
                    ;;
            esac
            ;;
    esac
}

_msc "$@"
`, agentList)

	case "fish":
		fmt.Printf(`complete -c msc -f
complete -c msc -l help -s h -d "Show help"
complete -c msc -l version -s v -d "Show version"
complete -c msc -l debug -s d -d "Enable debug logging"
complete -c msc -l quiet -s q -d "Suppress msc output"
complete -c msc -l dry-run -s n -d "Show resolved config without launching"
complete -c msc -l json -s j -d "Output as JSON"
complete -c msc -l force -s f -d "Launch even if MuninnDB is unreachable"
complete -c msc -l no-inject -d "Disable memory injection"
complete -c msc -l no-auto-calibrate -d "Keep the injection threshold fixed"
complete -c msc -l inject-budget -r -d "Max tokens to inject per request"
complete -c msc -l inject-min-score -r -d "Min cosine to inject a memory (0-1)"
complete -c msc -l recall-mode -r -a "semantic recent balanced deep" -d "MuninnDB recall mode"
complete -c msc -l log-json -d "Emit logs as JSON"
complete -c msc -l vault -r -d "MuninnDB vault name"
complete -c msc -l mcp-url -r -d "MuninnDB MCP endpoint"
complete -c msc -l token -r -d "MuninnDB bearer token"
`)
		// Build the "no subcommand given yet" condition. The standard fish
		// function __fish_seen_subcommand_from returns true when one of the
		// listed words has already appeared, so negating it guards completions
		// that should only appear as the first positional argument.
		allCmds := append([]string{"list", "status", "version", "help", "completion"}, names...)
		noSubcmd := fmt.Sprintf("not __fish_seen_subcommand_from %s", strings.Join(allCmds, " "))
		// Only complete agent names and subcommands for the first positional argument.
		for _, n := range names {
			fmt.Printf("complete -c msc -n '%s' -a %s -d \"Proxy %s API traffic\"\n", noSubcmd, n, n)
		}
		fmt.Printf("complete -c msc -n '%s' -a list -d \"List supported agents\"\n", noSubcmd)
		fmt.Printf("complete -c msc -n '%s' -a status -d \"Check MuninnDB connectivity\"\n", noSubcmd)
		fmt.Printf("complete -c msc -n '%s' -a version -d \"Show version\"\n", noSubcmd)
		fmt.Printf("complete -c msc -n '%s' -a help -d \"Show help\"\n", noSubcmd)
		fmt.Printf("complete -c msc -n '%s' -a completion -d \"Generate shell completions\"\n", noSubcmd)
		// Shell completions for the completion subcommand.
		fmt.Println(`complete -c msc -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish' -d "Shell type"`)

	default:
		logerr("unsupported shell: %s (use bash, zsh, or fish)", shell)
		return exitUsage
	}
	return 0
}
