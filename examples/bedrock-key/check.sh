#!/bin/sh
# Known-answer check for bedrock-key: fixed AWS example credentials and time.
# The signature was cross-checked against yoke/pkg/provider/aws SigningKey +
# StringToSign (themselves pinned to the AWS SigV4 test vector) over the same
# canonical request with the empty-payload hash.
set -eu
cd "$(dirname "$0")"
got=$(env -u AWS_SESSION_TOKEN AWS_ACCESS_KEY_ID=AKIDEXAMPLE \
	AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY AWS_REGION=us-east-1 \
	bashy --bashsharp bedrock-key.bsh --at 1700000000 | sed 's/^bedrock-api-key-//' | base64 -d)
want='bedrock.amazonaws.com/?Action=CallWithBearerToken&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDEXAMPLE%2F20231114%2Fus-east-1%2Fbedrock%2Faws4_request&X-Amz-Date=20231114T221320Z&X-Amz-Expires=43200&X-Amz-SignedHeaders=host&X-Amz-Signature=8a84612423da34d1a3f6105548d8270e4795d74a1446eb96b671f17b1478ac3d&Version=1'
[ "$got" = "$want" ] || { echo "bedrock-key: mismatch"; echo "got:  $got"; echo "want: $want"; exit 1; }
# Same key from a shared-credentials profile when no AWS CLI file is on PATH.
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
printf '[default]\naws_access_key_id = WRONG\naws_secret_access_key = WRONG\n\n[door]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY\n' > "$tmp/credentials"
got=$(env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN PATH="$(dirname "$(command -v bashy)"):/usr/bin:/bin" \
	AWS_PROFILE=door AWS_SHARED_CREDENTIALS_FILE="$tmp/credentials" AWS_REGION=us-east-1 \
	bashy --bashsharp bedrock-key.bsh --at 1700000000 | sed 's/^bedrock-api-key-//' | base64 -d)
[ "$got" = "$want" ] || { echo "bedrock-key: profile-file mismatch"; echo "got:  $got"; exit 1; }
echo "bedrock-key: ok"
