Register-ArgumentCompleter -Native -CommandName __APPNAME__ -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)

  $elements = $commandAst.CommandElements
  $completionArgs = @()

  # Extract each of the arguments
  for ($i = 0; $i -lt $elements.Count; $i++) {
    $completionArgs += $elements[$i].Extent.Text
  }

  # Add empty string if there's a trailing space (wordToComplete is empty but cursor is after space)
  # Necessary for differentiating between getting completions for namespaced commands vs. subcommands
  if ($wordToComplete.Length -eq 0 -and $elements.Count -gt 0) {
    $completionArgs += ""
  }

  $output = & {
    $env:COMPLETION_STYLE = 'pwsh'
    __APPNAME__ __complete @completionArgs 2>&1
  }
  $exitCode = $LASTEXITCODE

  # Check for custom file completion patterns
  # Patterns can appear anywhere in the word (e.g., inside quotes: 'my file is @file://path')
  $prefix = ""
  $filePart = $wordToComplete
  $forceFileCompletion = $false

  # PowerShell includes quotes in $wordToComplete - strip them for pattern matching;
  # $toCompletionText re-quotes the result
  $wordContent = $wordToComplete
  $leadingQuote = ""
  if ($wordToComplete -match '^([''"])(.*)(\1)$') {
    # Fully quoted: "content" or 'content'
    $leadingQuote = $Matches[1]
    $wordContent = $Matches[2]
  } elseif ($wordToComplete -match '^([''"])(.*)$') {
    # Opening quote only: "content or 'content
    $leadingQuote = $Matches[1]
    $wordContent = $Matches[2]
  }
  # Globbing and re-escaping below need the literal text, not the quoted form's inner escapes.
  if ($leadingQuote -eq "'") {
    $wordContent = $wordContent -replace "''", "'"
  } elseif ($leadingQuote -eq '"') {
    $wordContent = $wordContent -replace '`(.)|"(")', '$1$2'
  }

  if ($wordContent -match '^(.*)@(file://|data://)?(.*)$') {
    $prefix = $Matches[1] + '@' + $Matches[2]
    $filePart = $Matches[3]
    $forceFileCompletion = $true
  }

  # CompletionText is spliced into the command line verbatim, and `;` `&` `$(` and spaces are all
  # legal in filenames: emit anything that is not plainly inert as an escaped single-quoted literal.
  $toCompletionText = {
    param([string]$Text)
    if ($leadingQuote -eq '' -and $Text -match '\A[\w\-./\\:~]+\z') { return $Text }
    "'" + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($Text) + "'"
  }

  # CompletionText replaces the whole word, so each result carries the typed prefix and directory,
  # not just the leaf name Get-ChildItem returns.
  $completeFiles = {
    param([string]$Typed, [string]$Prefix)
    $typedDir = if ($Typed -match '^(.*[\\/])') { $Matches[1] } else { '' }
    $items = if ([string]::IsNullOrEmpty($Typed)) {
      Get-ChildItem -ErrorAction SilentlyContinue
    } else {
      Get-ChildItem -Path "$Typed*" -ErrorAction SilentlyContinue
    }
    $items | ForEach-Object {
      $itemText = $Prefix + $typedDir + $_.Name + $(if ($_.PSIsContainer) { '/' } else { '' })
      [System.Management.Automation.CompletionResult]::new(
        (& $toCompletionText $itemText),
        $itemText,
        'ProviderItem',
        $itemText
      )
    }
  }

  if ($forceFileCompletion) {
    & $completeFiles $filePart $prefix
  } else {
    switch ($exitCode) {
      10 {
        & $completeFiles $wordContent ''
      }
      11 {
        # No reasonable suggestions
        [System.Management.Automation.CompletionResult]::new(' ', ' ', 'ParameterValue', ' ')
      }
      default {
        # Default behavior - show command completions
        $output | ForEach-Object {
          [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
      }
    }
  }
}
