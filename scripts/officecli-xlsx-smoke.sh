#!/bin/sh
set -eu

export OFFICECLI_SKIP_UPDATE=1
export OFFICECLI_NO_AUTO_RESIDENT=1

workbook=/tmp/officecli-xlsx-smoke.xlsx
trap 'rm -f "$workbook"' EXIT

officecli create "$workbook"
officecli batch "$workbook" --commands '[
  {"command":"set","path":"/Sheet1/A1","props":{"value":"smoke text","bold":"true","fill":"FFF2CC"}},
  {"command":"set","path":"/Sheet1/A2","props":{"value":"=1+1"}},
  {"command":"set","path":"/Sheet1/A3","props":{"value":""}}
]'

a1="$(officecli get "$workbook" /Sheet1/A1 --json)"
a2="$(officecli get "$workbook" /Sheet1/A2 --json)"
a3="$(officecli get "$workbook" /Sheet1/A3 --json)"
validation="$(officecli validate "$workbook" --json)"

assert_json() {
  json=$1
  filter=$2
  if ! printf '%s\n' "$json" | jq -e "$filter" >/dev/null; then
    printf '%s\n' "$json" >&2
    return 1
  fi
}

assert_json "$a1" '
  .success == true and
  .data.results[0].path == "/Sheet1/A1" and
  .data.results[0].text == "smoke text" and
  (.data.results[0].format["font.bold"] == true or .data.results[0].format["font.bold"] == "true") and
  (.data.results[0].format.fill | ascii_downcase | test("^#?fff2cc$"))
'
assert_json "$a2" '
  .success == true and
  .data.results[0].path == "/Sheet1/A2" and
  (.data.results[0].format.formula == "=1+1" or .data.results[0].format.formula == "1+1") and
  (.data.results[0].format.computedValue == "2" or .data.results[0].format.cachedValue == "2" or .data.results[0].text == "2")
'
assert_json "$a3" '
  .success == true and
  .data.results[0].path == "/Sheet1/A3" and
  .data.results[0].text == ""
'
assert_json "$validation" '
  .success == true and
  .data.count == 0 and
  (.data.errors | length == 0)
'
