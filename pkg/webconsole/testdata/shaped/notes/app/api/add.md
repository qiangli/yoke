# add

Append the request body, as one line, to the caller's notes.

```bsh
if [ "$REQUEST_METHOD" != POST ]; then
  echo '{"error":"POST a note"}'
  exit 2
fi
note=$(printf '%s' "$BASHY_REQUEST_BODY" | tr -d '\r\n')
if [ -z "$note" ]; then
  echo '{"error":"empty note"}'
  exit 1
fi
printf '%s\n' "$note" >> "$BASHY_APP_DATA/notes.txt"
echo '{"ok":true}'
```
