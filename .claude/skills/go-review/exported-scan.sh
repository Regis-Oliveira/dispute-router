#!/bin/bash
# Lists exported names under internal/ that no other package references.
# A type reached only through an exported constructor or signature is reported
# as fine; a func, var or const that only its own package uses is a candidate
# to unexport. Enum constants of exported types and Err sentinels are reported
# too: keep those exported, the scan cannot tell them apart.
cd "$(git rev-parse --show-toplevel)" || exit 1
for dir in $(find internal -type d); do
  gofiles=$(ls $dir/*.go 2>/dev/null | grep -v _test.go); [ -z "$gofiles" ] && continue
  pkg=$(grep -h -m1 '^package ' $gofiles | head -1 | awk '{print $2}')
  # kind + name for exported top-level decls (incl. grouped blocks)
  decls=$( { grep -hoE '^(func|type|var|const) +[A-Z][A-Za-z0-9_]*' $gofiles | awk '{print $1":"$2}';
             awk '/^(type|const|var) \($/{kind=$1;inb=1;next} inb&&/^\)/{inb=0} inb&&/^\t[A-Z][A-Za-z0-9_]*[ \t=]/{print kind":"$1}' $gofiles; } | sort -u)
  # all exported signatures + exported struct fields in the package (where a type may legitimately need to be exported)
  sigs=$(grep -hE '^func (\([^)]*\) )?[A-Z]' $gofiles; awk '/^type [A-Z].*struct \{/{inb=1;next} inb&&/^\}/{inb=0} inb&&/^\t[A-Z]/{print}' $gofiles)
  for d in $decls; do
    kind=${d%%:*}; n=${d##*:}
    ext=$(grep -rlE "\b$pkg\.$n\b" --include='*.go' . | grep -v "^./$dir/[^/]*$" | wc -l | tr -d ' ')
    [ "$ext" != "0" ] && continue
    exttest=$(grep -lE "\b$pkg\.$n\b" $dir/*_test.go 2>/dev/null | wc -l | tr -d ' ')
    if [ "$kind" = "type" ]; then
      insig=$(echo "$sigs" | grep -cE "\b$n\b")
      if [ "$insig" != "0" ]; then reason="type reached via exported signature/field (ok to keep exported)"; else reason="TYPE not in any exported signature -> candidate to unexport"; fi
    else
      reason="$kind never used outside package -> candidate to unexport"
    fi
    [ "$exttest" != "0" ] && reason="$reason [used by external _test in same dir]"
    echo "$dir: $n  -> $reason"
  done
done
