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
want='bedrock.amazonaws.com/?Action=CallWithBearerToken&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDEXAMPLE%2F20231114%2Fus-east-1%2Fbedrock%2Faws4_request&X-Amz-Date=20231114T221320Z&X-Amz-Expires=43200&X-Amz-SignedHeaders=host&X-Amz-Signature=fe413fbf739fa3aad668e9a0d0694b75a695c35aadaf98be532d1e9ac54702a2&Version=1'
[ "$got" = "$want" ] || { echo "bedrock-key: mismatch"; echo "got:  $got"; echo "want: $want"; exit 1; }
echo "bedrock-key: ok"
