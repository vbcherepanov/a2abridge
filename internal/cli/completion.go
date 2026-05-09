package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func init() {
	registerCommand(Command{
		Name:    "completion",
		Summary: "Print a shell completion script (bash|zsh|fish|powershell)",
		Run:     RunCompletion,
	})
}

// RunCompletion emits a tab-completion script. Usage examples:
//
//	a2abridge completion bash >> ~/.bashrc
//	a2abridge completion zsh  > ~/.zsh/completions/_a2abridge
//	a2abridge completion fish > ~/.config/fish/completions/a2abridge.fish
//	a2abridge completion powershell | Out-String | Invoke-Expression
func RunCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: a2abridge completion <bash|zsh|fish|powershell>")
		return 2
	}
	shell := strings.ToLower(args[0])
	cmds := subcommandList()

	// Always offer "help" — it's a router-level alias, not a registered
	// subcommand, so it isn't in `commands`. "version" already is.
	cmds = append(cmds, "help")
	sort.Strings(cmds)

	switch shell {
	case "bash":
		fmt.Fprint(stdout, bashScript(cmds))
	case "zsh":
		fmt.Fprint(stdout, zshScript(cmds))
	case "fish":
		fmt.Fprint(stdout, fishScript(cmds))
	case "powershell", "pwsh":
		fmt.Fprint(stdout, powershellScript(cmds))
	default:
		fmt.Fprintf(stderr, "completion: unsupported shell %q (want: bash|zsh|fish|powershell)\n", shell)
		return 2
	}
	return 0
}

// subcommandList returns sorted top-level subcommand names. Used by every
// shell template — bash/zsh/fish/PowerShell all just need the list.
func subcommandList() []string {
	out := make([]string, 0, len(commands))
	for n := range commands {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func bashScript(cmds []string) string {
	return fmt.Sprintf(`# a2abridge bash completion
_a2abridge_complete() {
  local cur
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=( $(compgen -W "%s" -- "$cur") )
  fi
}
complete -F _a2abridge_complete a2abridge
`, strings.Join(cmds, " "))
}

func zshScript(cmds []string) string {
	return fmt.Sprintf(`#compdef a2abridge
# a2abridge zsh completion
_a2abridge() {
  local -a subcommands
  subcommands=(%s)
  if (( CURRENT == 2 )); then
    _describe 'subcommand' subcommands
  fi
}
compdef _a2abridge a2abridge
`, strings.Join(cmds, " "))
}

func fishScript(cmds []string) string {
	var b strings.Builder
	b.WriteString("# a2abridge fish completion\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "complete -c a2abridge -f -n '__fish_use_subcommand' -a '%s'\n", c)
	}
	return b.String()
}

func powershellScript(cmds []string) string {
	return fmt.Sprintf(`# a2abridge PowerShell completion
Register-ArgumentCompleter -CommandName a2abridge -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)
  $subs = @(%s)
  $subs | Where-Object { $_ -like "$wordToComplete*" } |
    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`, "'"+strings.Join(cmds, "' '")+"'")
}
