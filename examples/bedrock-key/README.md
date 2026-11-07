# bedrock-key — AWS Bedrock short-term keys, minted at launch

`bedrock-key.bsh` prints an AWS Bedrock short-term API key (at most 12 hours,
bound to one region) minted from this host's AWS credentials. A `~~~go` fence
(standard library only) computes it locally; nothing is called or stored. It is
registered as a bashy command and bound in `secrets.map` with the `!CMD` form,
so every launch of an agent on a Bedrock model gets a fresh key.

```sh
# 1. register the command (once per host)
bashy commands add bedrock-key --set exec.0=bashy --set exec.1=--bashsharp \
    --set exec.2="$PWD/bedrock-key.bsh" \
    --set synopsis="mint an AWS Bedrock short-term API key"

# 2. bind the aws-bedrock credential ref to it (host-local)
echo 'AWS_BEDROCK_API_KEY=!bashy bedrock-key' >> ~/.config/bashy/secrets.map

# 3. use any Bedrock agent; the key is minted per launch
bashy agent run ycode-bedrock-glm-5 "say ok"
```

The Bedrock model seeds name `api_key_ref: aws-bedrock`. Each tool projects the
one resolved value under its own name: `AWS_BEARER_TOKEN_BEDROCK` (claude,
codex, opencode), `ZCODE_API_KEY` (zcode), `OPENAI_API_KEY` (genie), or the
`key_env` of any other OpenAI-compatible tool. The AWS credentials stay with the
command and never enter the agent's environment.

Credentials come from `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`
(`AWS_SESSION_TOKEN`), else the AWS CLI chain (profile, SSO, role) via
`bashy aws configure export-credentials`, else (no AWS CLI installed) the
static keys of profile `AWS_PROFILE` (default `default`) in the shared
credentials file (`AWS_SHARED_CREDENTIALS_FILE`, default `~/.aws/credentials`). The region is `AWS_REGION`, else
`AWS_DEFAULT_REGION`, else `us-east-1`, and must match the model's endpoint.
`--expires SECONDS` (1..43200, default 43200) shortens the key.

`check.sh` is the offline known-answer check; its signature was cross-checked
against `pkg/provider/aws` `SigningKey`/`StringToSign`. Verified live
2026-11-07 against bedrock-runtime us-east-1: only a POST presign with the
empty-payload SHA-256 authenticates (GET, or S3's `UNSIGNED-PAYLOAD`, gets 401
"API Key is valid"). So `pkg/provider/aws.PresignURL` (GET-style, unsigned
payload) is not a substitute.
