package main

import (
	"context"
	"fmt"
	"io"
)

// completionShells are the shells 'linea completion' writes a script for.
var completionShells = []string{"bash", "fish", "powershell", "zsh"}

const completionSynopsis = "bash|zsh|fish|powershell"

// completionCmd prints the completion script for one shell.
func completionCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "usage: linea completion %s\n\n%s", completionSynopsis, completionHelp)
	}
	if len(args) == 1 {
		switch args[0] {
		case "-h", "-help", "--help", "help":
			usage(stdout)
			return exitOK
		}
		if script, ok := completionScripts[args[0]]; ok {
			fmt.Fprint(stdout, script)
			return exitOK
		}
		fmt.Fprintf(stderr, "unknown shell %q\n", args[0])
	}
	usage(stderr)
	return exitError
}

const completionHelp = `Prints a script that completes linea's commands, subcommands, flags and
flag values. Load it in the current shell, or add the line to your profile:

  bash        source <(linea completion bash)                    (~/.bashrc)
  zsh         source <(linea completion zsh)                     (~/.zshrc, after compinit)
  fish        linea completion fish | source                     (~/.config/fish/config.fish)
  powershell  linea completion powershell | Out-String | Invoke-Expression   ($PROFILE)
`

var completionScripts = map[string]string{
	"bash": `# bash completion for linea: source <(linea completion bash)
_linea_complete() {
    local line=${COMP_LINE:0:COMP_POINT} cur="" out directive c prefix=""
    local -a words
    read -ra words <<< "$line"
    if [[ $line != *[[:space:]] ]]; then
        cur=${words[${#words[@]}-1]}
        unset 'words[${#words[@]}-1]'
    fi
    out=$("${words[0]}" __complete "${words[@]:1}" "cur:$cur" 2>/dev/null) || return
    directive=${out##*$'\n'}
    out=${out%"$directive"}
    # bash completes the value after "--name=" as a word of its own.
    if [[ $cur == --*=* && $COMP_WORDBREAKS == *=* ]]; then
        prefix=${cur%%=*}=
    fi
    COMPREPLY=()
    while IFS= read -r c; do
        [[ -n $c ]] && COMPREPLY+=("${c#"$prefix"}")
    done <<< "$out"
    case $directive in
        :files) compopt -o filenames 2>/dev/null; mapfile -t -O "${#COMPREPLY[@]}" COMPREPLY < <(compgen -f -- "$cur") ;;
        :dirs) compopt -o filenames 2>/dev/null; mapfile -t -O "${#COMPREPLY[@]}" COMPREPLY < <(compgen -d -- "$cur") ;;
    esac
}
complete -F _linea_complete linea linea.exe
`,
	"zsh": `#compdef linea
# zsh completion for linea: source <(linea completion zsh), after compinit
_linea() {
    local out directive
    local -a lines cands
    out=$(${words[1]} __complete "${(@)words[2,CURRENT-1]}" "cur:${words[CURRENT]}" 2>/dev/null) || return
    lines=("${(@f)out}")
    directive=${lines[-1]}
    cands=("${(@)lines[1,-2]}")
    (( ${#cands} )) && compadd -Q -- "${cands[@]}"
    case $directive in
        :files) _files ;;
        :dirs) _files -/ ;;
    esac
}
compdef _linea linea
`,
	"fish": `# fish completion for linea: linea completion fish | source
function __linea_complete
    set -l tokens (commandline -opc)
    set -l program $tokens[1]
    set -e tokens[1]
    set -l cur (commandline -ct)
    set -l out ($program __complete $tokens "cur:$cur" 2>/dev/null)
    or return
    set -l directive $out[-1]
    set -e out[-1]
    printf '%s\n' $out
    switch $directive
        case :files
            __fish_complete_path $cur
        case :dirs
            __fish_complete_directories $cur
    end
end
complete -c linea -f -a '(__linea_complete)'
`,
	"powershell": `# PowerShell completion for linea: linea completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName 'linea', 'linea.exe' -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $elements = @($commandAst.CommandElements | Where-Object { $_.Extent.StartOffset -lt $cursorPosition })
    $program = $elements[0].Extent.Text.Trim([char[]]"'""")
    $before = @($elements | Select-Object -Skip 1 | ForEach-Object { $_.Extent.Text })
    if ($wordToComplete -ne '' -and $before.Count -gt 0) {
        $before = @($before | Select-Object -SkipLast 1)
    }
    # The word being completed is marked so that it is never an empty argument.
    $out = @(& $program __complete @before "cur:$wordToComplete" 2>$null)
    if ($out.Count -lt 2) {
        # No candidates: PowerShell offers paths, which suits :files and :dirs.
        return
    }
    foreach ($c in $out[0..($out.Count - 2)]) {
        [System.Management.Automation.CompletionResult]::new($c, $c, 'ParameterValue', $c)
    }
}
`,
}
