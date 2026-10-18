# Sprint 457 conditional CLI license dispositions and restricted-runtime alternatives

Decision document and pinned license evidence for **Story #2299 (`ec57e40986fa`)**, dated 2026-10-18.
This is a **SPIKE**: output is a formal disposition record and pinned evidence catalog. No client code is added and no software is installed.

Governing contracts:
- [Sprint 457 client admission and shared CLI/MCP contract](sprint-457-client-admission-contract.md) (`f0f50ffd00fa`)
- [Sprint 457 master execution plan](sprint-457-master-execution-plan.md) (Sprint UUID: `92c2b14f-0a6f-5f06-ac3c-2f83deb16dae`)
- [Bashy service supply-chain policy](bashy-service-supply-chain-policy.md)
- Research source data: `docs/research/n8n-cli/candidates.csv` and `docs/research/n8n-cli/license-evidence.json`

---

## 1. Executive summary and disposition matrix

Under the Sprint 457 admission contract and Bashy supply-chain policy, software admitted to the agentic userland must satisfy strict license, execution-mode, and dependency boundaries:
- **Rule 1 (managed servers / long-lived services):** requires OSI-permissive source and Bashy-built signed binaries from pinned release tags. Copyleft, source-available, or restricted-license software can never satisfy Rule 1.
- **Rule 2 (short-lived CLI binaries):** requires permissive licenses (Apache-2.0, MIT, BSD, PostgreSQL). No admission may be inferred from historical licenses (e.g. legacy BSD Redis), and promises of future license transitions (e.g. Sentry FSL conversion) cannot be admitted as permissive today.
- **Redundant transports & heavy runtimes:** Transports that duplicate qualified tools (e.g., `curl` in `external/curlbin`) or developer lifecycle CLIs requiring heavy multi-megabyte language runtimes (Node.js, Python, PHP) are excluded or deferred in favor of direct API adapters.

All nine conditional candidates from the n8n CLI research catalog (`candidates.csv`) are evaluated below. Explicit dispositions are recorded: **integrate**, **defer**, or **exclude**.

| # | Candidate | Assessed license | Classification | Disposition | Recommended alternative / route |
|---|---|---|---|---|---|
| 1 | **mariadb** | GPL-2.0-only | Non-permissive: copyleft | **exclude** | Pure-Go MySQL/MariaDB wire client or direct database/admin API adapter |
| 2 | **redis-cli** | RSALv2 OR SSPL-1.0 OR AGPL-3.0-only | Non-permissive: restricted / copyleft choice | **exclude** | **valkey-cli** (BSD-3-Clause; already integrated in `external/dataclients`) |
| 3 | **databricks** | LicenseRef-Databricks | Non-permissive: restricted proprietary | **exclude** | Databricks REST API (Jobs, SQL, Workspace) via typed HTTP/OAuth adapter |
| 4 | **sentry-cli** | FSL-1.1-MIT | Non-permissive: restricted commercial non-compete | **exclude** (sentry-cli) / **defer** (getsentry/toolkit) | Direct Sentry REST API for releases/sourcemaps; defer toolkit audit |
| 5 | **supabase** | MIT (`apps/cli-go`) / Undeclared scope (`apps/cli`) | Mixed/unclear package scope | **defer** | Direct `psql` for database queries; PostgREST and Management REST APIs |
| 6 | **wp** (WP-CLI) | MIT (wrapper only; couples to GPL WordPress) | Permissive wrapper, copyleft runtime | **defer** | Official WordPress REST API (`/wp-json/wp/v2/`) over HTTP |
| 7 | **shopify** | MIT | Permissive (developer lifecycle scope) | **defer** | Shopify Admin GraphQL/REST APIs for business data (orders, products) |
| 8 | **hf** (Hugging Face) | Apache-2.0 (client only; model weights separate) | Permissive client, heterogeneous assets | **defer** | Hugging Face Serverless Inference REST API; S11 locators + license check |
| 9 | **httpie** | BSD-3-Clause | Permissive (redundant transport) | **exclude** | Qualified `curl` (`external/curlbin`, certified coreutils) and pure-Go HTTP |

---

## 2. Policy framework and admission rules

### 2.1 Mode classification: short-lived client vs. long-lived service
The admission contract classifies execution boundaries before enabling any client:
- **Short-lived client (Rule 2):** Single bounded execution against an explicit remote endpoint; exits on completion or timeout; leaves no daemon, listener, or resident child.
- **Managed service or long-lived daemon (Rule 1):** Resident processes, webhook listeners, background pollers, or daemon containers require Rule 1 compliance (permissive source, Bashy-built signed binary, SBOM, telemetry disabled).
- **External user-managed endpoint:** Clients connect to user-supplied endpoints. Admission of a client does *not* grant permission to install, provision, or start the corresponding server.

### 2.2 Strict licensing principles
- **No historical license admission:** Software that previously used a permissive license but has relicensed to non-permissive terms (e.g. Redis switching from BSD-3-Clause to RSALv2/SSPL/AGPL) cannot be admitted under historical claims. The evaluated upstream release snapshot governs.
- **No future-promise admission:** Software released under source-available or restricted terms with an eventual delayed conversion to permissive terms (e.g. Sentry's Functional Source License converting to MIT after two years) remains non-permissive until the actual conversion occurs.
- **Separation of tool and runtime/asset licenses:**
  - A permissive CLI wrapper (e.g. WP-CLI under MIT) does not license the underlying server runtime (WordPress core under GPL-2.0-or-later).
  - A permissive asset-retrieval client (e.g. Hugging Face Hub under Apache-2.0) does not license the downloaded machine learning models, weights, or datasets, which frequently carry non-commercial (CC-BY-NC), OpenRAIL, or proprietary license terms.
- **Redundancy and dependency policy:** Adding an external CLI binary that duplicates existing certified userland tools (e.g. HTTPie duplicating `curl`) or introduces heavyweight language runtime environments (Node.js, Python, PHP) without unique service-level protocol functionality violates KISS and bloats the supply chain. Direct typed API adapters or pure-Go wire clients are preferred.

---

## 3. Detailed candidate assessments and dispositions

### 3.1 MariaDB CLI (`mariadb`)
- **Upstream candidate:** `MariaDB/server` (`mariadb`, `mariadb-dump`, `mysql`)
- **Pinned commit:** `327437ade1d6dc6e4e57a6f1723b4abe4ac6c4b4`
- **License text URL:** [MariaDB/server COPYING](https://github.com/MariaDB/server/blob/327437ade1d6dc6e4e57a6f1723b4abe4ac6c4b4/COPYING)
- **License SHA-256:** `240a15a1d0f34d3abca462cdb7e5fb89470967563f16b0e71169e51c1e74cf2b`
- **License expression:** `GPL-2.0-only` (SPDX: `GPL-2.0-only`)
- **Analysis:**
  - MariaDB CLI is distributed as part of the MariaDB server tree under GNU General Public License Version 2 only.
  - As a strong copyleft license, it cannot be linked into, embedded in, or shipped with pure-Go Yoke/Bashy userland components.
  - Invoking MariaDB CLI as a host-only external binary under Rule 2 requires provisioning bloated C packages and managing complex platform packaging.
  - The MySQL/MariaDB wire protocol is an open, standardized protocol. Permissive alternatives exist: pure-Go drivers (`go-sql-driver/mysql`, MPL-2.0 / permissive) or direct REST/HTTP endpoints exposed by modern cloud databases (PlanetScale, MariaDB SkySQL).
- **Explicit disposition:** **`exclude`** MariaDB CLI binary.
- **Alternative:** Implement a lightweight, pure-Go wire client or direct HTTP API adapter for MariaDB/MySQL compatible endpoints under Sprint 457 `cf3c288d595a` (typed API adapters) or future database rounds.

---

### 3.2 Current redis-cli (`redis-cli`) vs. Valkey client (`valkey-cli`)
- **Upstream candidate:** `redis/redis` (`src/redis-cli.c`)
- **Pinned commit:** `5e332cabd547d272783cf233a34c81dcb9ea44b3`
- **License text URL:** [redis/redis LICENSE.txt](https://github.com/redis/redis/blob/5e332cabd547d272783cf233a34c81dcb9ea44b3/LICENSE.txt)
- **License SHA-256:** `4a0e416b9537688f30dfe69ddaceb2ca64d96b7df02a0a6760d376890ddc4e40`
- **License expression:** `RSALv2 OR SSPL-1.0 OR AGPL-3.0-only` (Non-permissive: restricted / copyleft choice)
- **Analysis:**
  - Starting with Redis 8.0, Redis moved from BSD-3-Clause to a tri-license: Redis Source Available License v2 (RSALv2, non-compete commercial limitation), Server Side Public License v1 (SSPLv1, non-OSI copyleft), or GNU AGPLv3.
  - Inspection of `src/redis-cli.c` at the reviewed commit confirms that client tools are subject to the same tri-license.
  - Historical BSD terms applying to Redis <= 7.2 cannot be used to justify admission of current or future Redis CLI tools.
  - **Permissive protocol alternative:** The Linux Foundation's **Valkey** project maintains an entirely permissive, 100% wire-compatible fork:
    - Repository: `valkey-io/valkey`
    - Commit: `e1b4ec84554a0eedb865487a0cc0be3443cfbc63`
    - License text URL: [valkey-io/valkey COPYING](https://github.com/valkey-io/valkey/blob/e1b4ec84554a0eedb865487a0cc0be3443cfbc63/COPYING)
    - License SHA-256: `9a67d40ae9076cd60b9d228135a9c0ce5d60f84f0c54c472965d4e03fdeb8f8f`
    - License expression: `BSD-3-Clause` (OSI-approved, permissive).
    - Status: `valkey-cli` has already been integrated and qualified as a short-lived client in `external/dataclients` under Sprint 457 Story #2295 (`9f087594f442`, commit `2fda37c8`), handling Redis-compatible key/value/list/set commands over TLS with credential redaction and transaction rollbacks.
- **Explicit disposition:**
  - **`redis-cli`:** **`exclude`** (prohibited due to non-permissive tri-license; no historical fallback).
  - **`valkey-cli`:** **`integrate`** (already admitted and qualified as the canonical Redis-protocol client).

---

### 3.3 Databricks CLI (`databricks`)
- **Upstream candidate:** `databricks/cli`
- **Pinned commit:** `4244322a261413a52e28e9e6834ca6744291a69c`
- **License text URL:** [databricks/cli LICENSE](https://github.com/databricks/cli/blob/4244322a261413a52e28e9e6834ca6744291a69c/LICENSE)
- **License SHA-256:** `40a3993bc2adcdb4bf42ddd36e5d1d7c69e275a7a294a7b45222239c4ab5b637`
- **License expression:** `LicenseRef-Databricks` (Non-permissive: custom restricted proprietary)
- **Analysis:**
  - Databricks CLI is distributed under a proprietary corporate license. Section "Scope of Use" explicitly states:
    > "You may not use the Licensed Materials except in connection with your use of the Databricks Services pursuant to the Agreement... solely for the purposes of using the Licensed Materials within or connecting to the Databricks Services."
  - This is a non-open, contractual restriction tying software usage directly to an active Master Cloud Services Agreement (MCSA). It fails OSI and FSF definitions of software freedom and violates Bashy's permissive supply-chain policy.
  - Distributing or packaging the Databricks CLI binary would compromise the open nature of the Bashy ecosystem.
  - Databricks provides comprehensive, public REST APIs (Jobs API 2.1, SQL Execution API, Workspace API, Unity Catalog API) authenticated via OAuth or Personal Access Tokens (PAT).
- **Explicit disposition:** **`exclude`** Databricks CLI binary.
- **Alternative:** Implement typed REST API adapters (using Bashy's standard HTTP transport, curl, or the permissive Apache-2.0 `databricks-sdk-go`) under story `cf3c288d595a` for required Jobs and SQL query operations.

---

### 3.4 Sentry CLI (`sentry-cli`) & successor toolkit
- **Upstream candidate:** `getsentry/sentry-cli`
- **Pinned commit:** `73854dba44ebb68c644976b359ff736ebbc95fb7`
- **License text URL:** [getsentry/sentry-cli LICENSE](https://github.com/getsentry/sentry-cli/blob/73854dba44ebb68c644976b359ff736ebbc95fb7/LICENSE)
- **License SHA-256:** `f8171b7a645bca1eff1e69f911dc4c1fa93bf92a2f7d4d7fa24b4b7660c33041`
- **License expression:** `FSL-1.1-MIT` (Functional Source License 1.1, with future conversion to MIT)
- **Analysis:**
  - FSL-1.1-MIT contains an explicit commercial non-compete clause: "A Permitted Purpose is any purpose other than a Competing Use."
  - Although the license includes an automated transition to MIT two years after release date, the software is non-permissive today. Promissory future licenses cannot be admitted as permissive today.
  - Furthermore, upstream project documentation states that `sentry-cli` is effectively superseded and that new integrations should target `getsentry/toolkit`.
  - Core Sentry operations needed for agentic workflows (release management, source map uploads, issue triage) are directly available via the official Sentry REST API (`https://sentry.io/api/0/`) with bearer auth.
- **Explicit disposition:**
  - **`sentry-cli`:** **`exclude`** (restricted FSL-1.1-MIT license; deprecated upstream).
  - **`getsentry/toolkit`:** **`defer`** (requires independent licensing, dependency, and maturity review).
- **Alternative:** Use direct Sentry REST API calls via Bashy secret and HTTP transport for release management and issue monitoring.

---

### 3.5 Supabase CLI (`supabase`)
- **Upstream candidate:** `supabase/cli`
- **Pinned commit:** `f01410ec6ad5624a9aa8b7c1be8f7b2977c37f05`
- **License text URL:** [supabase/cli apps/cli-go/LICENSE](https://raw.githubusercontent.com/supabase/cli/f01410ec6ad5624a9aa8b7c1be8f7b2977c37f05/apps/cli-go/LICENSE)
- **License SHA-256:** `81f7d60afa4316010b1c0df8eb8f0c80b27586a86b72f1bde85e129bfd10d52a`
- **License expression:** `MIT for apps/cli-go; undeclared scope for apps/cli` (Mixed / unclear scope)
- **Analysis:**
  - Licensing ambiguity: The repository contains both a Go implementation (`apps/cli-go`, explicitly MIT licensed) and a newer TypeScript/Bun implementation (`apps/cli`, declaring MIT in `package.json` but lacking a scoped LICENSE file).
  - Functional scope mismatch: The primary purpose of Supabase CLI is local stack orchestration (`supabase start`, `supabase stop`), which deploys and manages an entire multi-container Docker environment (Postgres, Kong, PostgREST, GoTrue, Realtime, Storage, Inbucket). Starting local containers or managed services violates Rule 1 and local environment isolation contracts.
  - Automation requirements (database queries, table CRUD, migrations, storage) are handled remotely without local daemon orchestration:
    - Direct PostgreSQL queries: Fully covered by `psql` (admitted in `external/dataclients` under PostgreSQL license).
    - Table CRUD and RPC: Served by Supabase's PostgREST endpoint (`https://{project}.supabase.co/rest/v1/`).
    - Project administration: Served by the Supabase Management API (`https://api.supabase.com/v1/`).
- **Explicit disposition:** **`defer`** Supabase CLI binary admission.
- **Alternative:** Use `psql` for remote database execution and direct HTTPS calls for PostgREST and Management APIs.

---

### 3.6 WP-CLI (`wp`)
- **Upstream candidate:** `wp-cli/wp-cli`
- **Pinned commit:** `2278cf3f52518938de341d1902b9fd9eb718ffd9`
- **License text URL:** [wp-cli/wp-cli LICENSE](https://github.com/wp-cli/wp-cli/blob/2278cf3f52518938de341d1902b9fd9eb718ffd9/LICENSE)
- **License SHA-256:** `9d94b972f0ea32ceb45bce0ab5229e421eaa3933fd07178d3256c13bae000ae5`
- **License expression:** `MIT` (Permissive tool; runtime is GPL-2.0-or-later)
- **Analysis:**
  - WP-CLI itself is licensed under MIT.
  - However, WP-CLI is not a standalone remote client. It operates by bootstrapping a local WordPress installation directly through the PHP engine (`require_once wp-load.php`). It requires a host PHP runtime and executes inside the WordPress core codebase, which is licensed under `GPL-2.0-or-later`.
  - WooCommerce functionality (`wp wc`) requires installing third-party plugins that carry GPL-3.0 or proprietary licensing.
  - Remote automation (publishing blog posts, updating metadata, listing users) is standardly, robustly handled over HTTPS by the official **WordPress REST API** (`/wp-json/wp/v2/`) using Application Passwords or OAuth.
  - The REST API requires zero local PHP or WordPress dependencies and avoids runtime GPL coupling.
- **Explicit disposition:** **`defer`** WP-CLI binary integration.
- **Alternative:** Connect to remote WordPress sites via the WordPress REST API using standard HTTP transport and structured JSON.

---

### 3.7 Shopify CLI (`shopify`)
- **Upstream candidate:** `Shopify/cli`
- **Pinned commit:** `060e88b3b9d8c6ca01dfa50bbb442ce6470060e2`
- **License text URL:** [Shopify/cli LICENSE](https://github.com/Shopify/cli/blob/060e88b3b9d8c6ca01dfa50bbb442ce6470060e2/LICENSE)
- **License SHA-256:** `40614f57726c0804b64757fcf155ebfb59c9dddfc27cc17c7717e2993571c771`
- **License expression:** `MIT` (Permissive client; developer lifecycle scope)
- **Analysis:**
  - Shopify CLI is permissively licensed under MIT.
  - Operational mismatch: Shopify CLI is built for application and theme developers (`shopify app dev`, `shopify theme serve`). It is not an administrative or transactional business data client for store operations (querying orders, inventory management, customer support, catalog mutations).
  - Heavy runtime footprint: It requires Node.js, Ruby, and a massive npm dependency graph that imposes severe SBOM and vulnerability management overhead without delivering storefront CRUD capabilities.
  - Storefront and administrative automation is exposed via the **Shopify Admin GraphQL API** and REST Admin API using private app access tokens or OAuth.
- **Explicit disposition:** **`defer`** Shopify CLI binary integration.
- **Alternative:** Implement a scoped Shopify Admin GraphQL/REST API adapter under `cf3c288d595a` for business data CRUD.

---

### 3.8 Hugging Face CLI (`hf` / `huggingface_hub`)
- **Upstream candidate:** `huggingface/huggingface_hub` (`hf`)
- **Pinned commit:** `bb2ef7579c3d2dc2eb49b16ccf15a070fe5b7023`
- **License text URL:** [huggingface/huggingface_hub LICENSE](https://github.com/huggingface/huggingface_hub/blob/bb2ef7579c3d2dc2eb49b16ccf15a070fe5b7023/LICENSE)
- **License SHA-256:** `c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4`
- **License expression:** `Apache-2.0` (Permissive client; model weights carry separate licenses)
- **Analysis:**
  - The `huggingface_hub` client CLI is licensed under Apache-2.0.
  - Separation of client tool and asset terms:
    - The CLI tool license covers only repository synchronization, upload/download commands, and space management.
    - It does *not* grant rights to the downloaded model weights, dataset contents, or checkpoints. Model weights carry distinct, heterogeneous, and often non-permissive licenses (e.g. Llama Community License, OpenRAIL, CC-BY-NC-4.0, custom commercial licenses).
  - The CLI does not perform model execution or inference; agentic inference workflows rely on the Hugging Face Serverless Inference API (`https://api-inference.huggingface.co/models/{model_id}`).
  - Python runtime requirement: The CLI requires a Python environment and transitive dependencies.
- **Explicit disposition:** **`defer`** Hugging Face CLI binary; admit direct API inference.
- **Alternative:** Use the Hugging Face Serverless Inference REST API for model queries and text generation. For large model artifacts, use Sprint 446 S11 locators and direct curl transfer with per-model license attestation.

---

### 3.9 HTTPie (`httpie`)
- **Upstream candidate:** `httpie/cli`
- **Pinned commit:** `5b604c37c6c67e18e7c3e9aee6c88a8c22b98345`
- **License text URL:** [httpie/cli LICENSE](https://github.com/httpie/cli/blob/5b604c37c6c67e18e7c3e9aee6c88a8c22b98345/LICENSE)
- **License SHA-256:** `89a72f18e678d9db71553f9a30bc0c436c71a61496849af9cd477871b70c31ef`
- **License expression:** `BSD-3-Clause` (Permissive transport; redundant)
- **Analysis:**
  - HTTPie is permissively licensed under BSD-3-Clause.
  - Redundant transport: HTTPie is a user-facing HTTP CLI that duplicates transport mechanisms already certified and deeply integrated in Bashy/Yoke:
    - Pure-Go HTTP clients in `toolcmd` and `external/gws`
    - Certified coreutils transport
    - Pinned and qualified `curl` (`external/curlbin`, qualified under Story #2290 / `83739c5261a3`)
  - HTTPie provides no distinct service schemas, authentication protocols, or pagination semantics beyond raw HTTP formatting.
  - Running HTTPie requires packaging Python, `requests`, `pygments`, and associated dependencies, adding maintenance cost and vulnerability surface without increasing capability.
- **Explicit disposition:** **`exclude`** HTTPie as a managed client.
- **Alternative:** Use qualified `curl` or native Go HTTP transports.

---

## 4. Protocol and API alternative comparison

The table below contrasts the nine conditional CLI candidates against their recommended protocol/API alternatives:

| Candidate | Conditional CLI concern | Preferred protocol / API alternative | Alternative license | Execution mode & security posture |
|---|---|---|---|---|
| **mariadb** | GPL-2.0-only copyleft; bulky C distribution | Pure-Go MySQL driver / wire client | Permissive (MPL-2.0 / BSD) | Short-lived client to explicit endpoint; TLS by default; no copyleft code |
| **redis-cli** | RSALv2 / SSPL / AGPL tri-license | **valkey-cli** | BSD-3-Clause | Qualified in `external/dataclients`; explicit endpoint; canary-tested redaction |
| **databricks** | Restricted proprietary MCSA license | Databricks REST API (Jobs, SQL) | Direct HTTPS / Apache-2.0 SDK | Standard HTTPS; Bearer token / OAuth; least-privilege tokens |
| **sentry-cli** | FSL-1.1-MIT non-compete; deprecated tool | Sentry REST API (`/api/0/`) | Direct HTTPS | Token auth via `bashy secret`; bounded release & issue operations |
| **supabase** | Unclear package scope; Docker stack daemon | `psql` + PostgREST HTTPS API | PostgreSQL / Direct HTTPS | `psql` for SQL queries; PostgREST for table CRUD; zero Docker dependencies |
| **wp** | Coupled to local PHP + GPL WordPress core | WordPress REST API (`/wp-json/`) | Direct HTTPS | Application password / OAuth; clean JSON input/output; zero local PHP |
| **shopify** | Scoped to app dev; Node.js dependency tree | Shopify Admin GraphQL API | Direct HTTPS | Storefront / Admin OAuth; structured GraphQL mutations for order/product data |
| **hf** | Repo sync only; model weights separate license | HF Serverless Inference REST API | Direct HTTPS (assets audited separately) | HF token auth; inference via HTTP; S11 locator with explicit license attestation |
| **httpie** | Redundant transport; Python runtime overhead | Qualified `curl` / native Go HTTP | curl license (MIT-like) / Pure Go | Zero extra runtime overhead; qualified in Sprint 457 Story #2290 |

---

## 5. Pinned license evidence catalog

The pinned evidence below is synchronized with `docs/research/n8n-cli/license-evidence.json` and validated by `validate.py`:

```json
[
  {
    "candidate": "mariadb",
    "repository": "MariaDB/server",
    "commit": "327437ade1d6dc6e4e57a6f1723b4abe4ac6c4b4",
    "license_url": "https://github.com/MariaDB/server/blob/327437ade1d6dc6e4e57a6f1723b4abe4ac6c4b4/COPYING",
    "license_sha256": "240a15a1d0f34d3abca462cdb7e5fb89470967563f16b0e71169e51c1e74cf2b",
    "license_spdx": "GPL-2.0-only",
    "disposition": "exclude"
  },
  {
    "candidate": "redis-cli",
    "repository": "redis/redis",
    "commit": "5e332cabd547d272783cf233a34c81dcb9ea44b3",
    "license_url": "https://github.com/redis/redis/blob/5e332cabd547d272783cf233a34c81dcb9ea44b3/LICENSE.txt",
    "license_sha256": "4a0e416b9537688f30dfe69ddaceb2ca64d96b7df02a0a6760d376890ddc4e40",
    "license_spdx": "RSALv2 OR SSPL-1.0 OR AGPL-3.0-only",
    "disposition": "exclude"
  },
  {
    "candidate": "valkey-cli",
    "repository": "valkey-io/valkey",
    "commit": "e1b4ec84554a0eedb865487a0cc0be3443cfbc63",
    "license_url": "https://github.com/valkey-io/valkey/blob/e1b4ec84554a0eedb865487a0cc0be3443cfbc63/COPYING",
    "license_sha256": "9a67d40ae9076cd60b9d228135a9c0ce5d60f84f0c54c472965d4e03fdeb8f8f",
    "license_spdx": "BSD-3-Clause",
    "disposition": "integrate"
  },
  {
    "candidate": "databricks",
    "repository": "databricks/cli",
    "commit": "4244322a261413a52e28e9e6834ca6744291a69c",
    "license_url": "https://github.com/databricks/cli/blob/4244322a261413a52e28e9e6834ca6744291a69c/LICENSE",
    "license_sha256": "40a3993bc2adcdb4bf42ddd36e5d1d7c69e275a7a294a7b45222239c4ab5b637",
    "license_spdx": "LicenseRef-Databricks",
    "disposition": "exclude"
  },
  {
    "candidate": "sentry-cli",
    "repository": "getsentry/sentry-cli",
    "commit": "73854dba44ebb68c644976b359ff736ebbc95fb7",
    "license_url": "https://github.com/getsentry/sentry-cli/blob/73854dba44ebb68c644976b359ff736ebbc95fb7/LICENSE",
    "license_sha256": "f8171b7a645bca1eff1e69f911dc4c1fa93bf92a2f7d4d7fa24b4b7660c33041",
    "license_spdx": "FSL-1.1-MIT",
    "disposition": "exclude"
  },
  {
    "candidate": "supabase",
    "repository": "supabase/cli",
    "commit": "f01410ec6ad5624a9aa8b7c1be8f7b2977c37f05",
    "license_url": "https://raw.githubusercontent.com/supabase/cli/f01410ec6ad5624a9aa8b7c1be8f7b2977c37f05/apps/cli-go/LICENSE",
    "license_sha256": "81f7d60afa4316010b1c0df8eb8f0c80b27586a86b72f1bde85e129bfd10d52a",
    "license_spdx": "MIT (scoped: apps/cli-go)",
    "disposition": "defer"
  },
  {
    "candidate": "wp",
    "repository": "wp-cli/wp-cli",
    "commit": "2278cf3f52518938de341d1902b9fd9eb718ffd9",
    "license_url": "https://github.com/wp-cli/wp-cli/blob/2278cf3f52518938de341d1902b9fd9eb718ffd9/LICENSE",
    "license_sha256": "9d94b972f0ea32ceb45bce0ab5229e421eaa3933fd07178d3256c13bae000ae5",
    "license_spdx": "MIT",
    "disposition": "defer"
  },
  {
    "candidate": "shopify",
    "repository": "Shopify/cli",
    "commit": "060e88b3b9d8c6ca01dfa50bbb442ce6470060e2",
    "license_url": "https://github.com/Shopify/cli/blob/060e88b3b9d8c6ca01dfa50bbb442ce6470060e2/LICENSE",
    "license_sha256": "40614f57726c0804b64757fcf155ebfb59c9dddfc27cc17c7717e2993571c771",
    "license_spdx": "MIT",
    "disposition": "defer"
  },
  {
    "candidate": "hf",
    "repository": "huggingface/huggingface_hub",
    "commit": "bb2ef7579c3d2dc2eb49b16ccf15a070fe5b7023",
    "license_url": "https://github.com/huggingface/huggingface_hub/blob/bb2ef7579c3d2dc2eb49b16ccf15a070fe5b7023/LICENSE",
    "license_sha256": "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4",
    "license_spdx": "Apache-2.0",
    "disposition": "defer"
  },
  {
    "candidate": "httpie",
    "repository": "httpie/cli",
    "commit": "5b604c37c6c67e18e7c3e9aee6c88a8c22b98345",
    "license_url": "https://github.com/httpie/cli/blob/5b604c37c6c67e18e7c3e9aee6c88a8c22b98345/LICENSE",
    "license_sha256": "89a72f18e678d9db71553f9a30bc0c436c71a61496849af9cd477871b70c31ef",
    "license_spdx": "BSD-3-Clause",
    "disposition": "exclude"
  }
]
```

---

## 6. Named unverified gaps (Sprint 465)

Consistent with the Sprint 457 contract, tests requiring real accounts, live SaaS credentials, or proprietary backend services are not attempted here and are recorded as named gaps for Sprint 465:
1. **Live API verification for alternatives:** Live authentication, rate limiting, and response schemas against live endpoints for Databricks REST API, Sentry REST API, Supabase PostgREST, WordPress REST API, Shopify Admin GraphQL API, and Hugging Face Serverless Inference API.
2. **Schema drift monitoring:** Automated checking of remote API breaking changes for direct REST/GraphQL endpoints.
3. **Per-model license assertion protocol:** Runtime verification tooling for Hugging Face model downloads ensuring non-commercial / restricted model weights are explicitly accepted by the user prior to local staging.

---

## 7. Umbrella documentation updates

### 7.1 Abstract for `docs/README.md`
```markdown
- `docs/sprint-457-conditional-cli-dispositions.md` — Sprint 457 Story #2299 (ec57e40986fa) spike decision document. Records explicit dispositions (integrate / defer / exclude) and pinned license evidence for nine conditional CLI candidates: excludes MariaDB CLI (GPL-2.0-only), current redis-cli (RSALv2/SSPL/AGPL tri-license), Databricks CLI (custom restricted MCSA license), sentry-cli (FSL-1.1-MIT), and HTTPie (redundant BSD-3-Clause transport); defers Supabase (mixed scope), WP-CLI (MIT tool coupled to GPL runtime), Shopify CLI (development lifecycle scope), and Hugging Face CLI (Apache-2.0 tool with separate model licenses); establishes valkey-cli (BSD-3-Clause, admitted) and direct typed REST/GraphQL APIs as preferred permissive protocol alternatives.
```

### 7.2 Index entry for `docs/INDEX.md`
```markdown
- `docs/sprint-457-conditional-cli-dispositions.md` — Sprint 457 Story #2299: explicit dispositions and pinned evidence for 9 conditional CLIs and permissive alternatives.
```
