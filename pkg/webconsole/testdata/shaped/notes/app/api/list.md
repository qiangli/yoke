# list

Every note in the caller's private data, oldest first, as a JSON array of
strings.

```bsh
f="$BASHY_APP_DATA/notes.txt"
printf '['
if [ -f "$f" ]; then
  awk '{ gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); printf "%s\"%s\"", (NR > 1 ? "," : ""), $0 }' "$f"
fi
printf ']\n'
```
