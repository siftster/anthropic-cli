#!/bin/bash

# readline hands completion the word minus shell quoting and, under `-o filenames`, re-quotes
# COMPREPLY for that context; text spliced in front of a filename has to be dequoted the same way.
____APPNAME___dequote() {
  local s=$1 q="" c i
  REPLY=""
  for ((i = 0; i < ${#s}; i++)); do
    c=${s:i:1}
    if [[ $c == "\\" && ( -z $q || ( $q == '"' && ${s:i+1:1} == [\$\`\"\\] ) ) ]]; then
      ((++i))
      REPLY+=${s:i:1}
    elif [[ -z $q && $c == [\'\"] ]]; then
      q=$c
    elif [[ $c == "$q" ]]; then
      q=""
    else
      REPLY+=$c
    fi
  done
}

____APPNAME___bash_autocomplete() {
  if [[ "${COMP_WORDS[0]}" != "source" ]]; then
    local cur completions exit_code
    local IFS=$'\n'
    cur="${COMP_WORDS[COMP_CWORD]}"

    completions=$(COMPLETION_STYLE=bash "${COMP_WORDS[0]}" __complete -- "${COMP_WORDS[@]:1:$COMP_CWORD-1}" "$cur" 2>/dev/null)
    exit_code=$?

    local last_token="$cur"

    # If the last token has been split apart by a ':', join it back together.
    # Ex: 'a:b' will be represented in COMP_WORDS as 'a', ':', 'b'
    if [[ $COMP_CWORD -ge 2 ]]; then
      local prev2="${COMP_WORDS[COMP_CWORD - 2]}"
      local prev1="${COMP_WORDS[COMP_CWORD - 1]}"
      if [[ "$prev2" =~ ^@(file|data)$ && "$prev1" == ":" && "$cur" =~ ^// ]]; then
        last_token="$prev2:$cur"
      fi
    fi

    # Check for custom file completion patterns
    local prefix=""
    local file_part="$cur"
    local force_file_completion=false
    if [[ "$last_token" =~ (.*)@(file://|data://)?(.*)$ ]]; then
      local before_at="${BASH_REMATCH[1]}"
      local protocol="${BASH_REMATCH[2]}"
      file_part="${BASH_REMATCH[3]}"
      ____APPNAME___dequote "$before_at"
      before_at=$REPLY

      if [[ "$protocol" == "" ]]; then
        prefix="$before_at@"
      elif [[ "$last_token" != "$cur" ]]; then
        # COMP_WORDBREAKS split `@file` `:` `//path` apart and readline replaces only the `//path` word.
        prefix="//"
      else
        prefix="$before_at@$protocol"
      fi

      force_file_completion=true
    fi

    # Without `-o filenames` readline splices COMPREPLY in verbatim, so a filename containing `;` or `$(`
    # becomes shell syntax; scoped to the file branches so command completions don't grow a trailing `/`.
    if [[ "$force_file_completion" == true ]]; then
      compopt -o filenames 2>/dev/null
      mapfile -t COMPREPLY < <(compgen -f -- "$file_part")
      local i
      for i in "${!COMPREPLY[@]}"; do COMPREPLY[i]="$prefix${COMPREPLY[i]}"; done
    else
      case $exit_code in
      10) compopt -o filenames 2>/dev/null; mapfile -t COMPREPLY < <(compgen -f -- "$cur") ;; # file completion
      11) COMPREPLY=() ;;                                   # no completion
      0) mapfile -t COMPREPLY <<<"$completions" ;;          # use returned completions
      esac
    fi
    return 0
  fi
}

complete -F ____APPNAME___bash_autocomplete __APPNAME__
