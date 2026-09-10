# The syllabus against the project, after the review

Source: `Ementa Detalhada — Engenharia de Software em IA Aplicada` (12 modules),
read in full on 2026-09-10 and set against the code as it is, not as the
documents describe it. The published syllabus artifact already does a version
of this; this file records where the review changes a verdict and which
strategies the artifact does not mention at all.

Verdicts: **built** · **fits** (would earn its place; the concrete shape is
given) · **after** (fits only once something else is true) · **no** (would be
wrong here, with the reason).

---

## Where the review changes the artifact's verdict

| strategy | artifact said | now | why |
|---|---|---|---|
| Prompt caching (Mód. 2, 8-U4) | fits, not built | **built** | commit 5545029; 24% measured. Artifact was stale. |
| RAG de precedentes (Mód. 8-U4) | measured, lost | **measured, undecided** | the "coverage" metric cannot move; set overlap says the strategies mostly return different precedents; no labels. See plan 5.3. |
| Guardrails "checked before each call" (Mód. 6-U11) | built | **partly** | true of loop.go; the drafting flow checks once, between calls, and the ceiling is per attempt. Plan 3.3 / 4.3. |
| Audit trail / prompt hash (Mód. 4-U7) | built | **partly** | the hash misses the record template, which is where every recent prompt change happened. Plan 4.5. |
| Injection test with counterfactual (Mód. 6-U1.6) | built | **built, no power** | the fixture asks for the answer the model gives anyway; the grader cannot fail. Plan 1.2. |

## Strategies the artifact does not mention, ranked by what they would buy

**1. Multimodal / OCR over the evidence files — Mód. 2 ("OCR inteligente para
extrair e entender documentos"). fits, and it is the biggest gap in the
domain.** The drafter sees `evidence_on_file` as names and sizes. A
representment is won by what the delivery confirmation *says*: the date, the
address, whether a signature is present. Shape: a host-side extraction step in
`FactSource.For` that runs a vision call per PDF/image and writes a structured
`evidence_contents` block into the record (fields, not prose), so both judges
see the same extraction. Two rules come with it: the extracted text is untrusted
(a receipt can carry a planted instruction as easily as a claim) and gets the
same fence as the claim; and the extraction is cached per S3 key + ETag in a
table, so a redraft does not pay twice. Cost is per file, not per draft.
The verifier's rule 3 then becomes checkable in substance, not only by filename.

**2. Hybrid search — Mód. 8-U4 ("Hybrid Search"). after.** Reciprocal-rank
fusion of `ts_rank` and cosine is ten lines of SQL over the two queries that
already exist, and it is the standard answer when neither strategy dominates.
But it needs the thing the comparison lacks: a label for which precedent was
right. Plan 5.3 first; label 30 cases by hand (the decision is "same argument
shape, same outcome relevance", a few minutes each); then hybrid is measured
against both, or not built.

**3. A reason-code corpus — Mód. 6-U10 ("RAG de runbooks"), Mód. 8-U4 (Basic
RAG). fits.** The one document retrieval the domain actually wants is not
precedent but the network's own rule text: what Visa 10.4 or Mastercard 4853
requires the merchant to show. A small table of reason-code guidance (hand
written from the public dispute guides, per network and code) rendered into
the record under its own heading gives the drafter the standard it is arguing
against and gives `gradeReasonCode` something better than a substring match.
No embeddings needed: the key is the reason code.

**4. Model tiering by amount in dispute — Mód. 8-U4/U5 (Model Router, Model
Tiering). fits, small.** `Provider` already returns the model id and every run
records it. Route by `amount_minor` band: a cheaper model below a threshold, the
current one above. The catch to say out loud: the eval is per model, so a router
means running it per tier, and `agent_runs.model` is what lets the pass rate be
split afterwards.

**5. Secret and IaC scanning — Mód. 6-U7 (Trivy, Checkov, secret leak
detection). fits, an afternoon.** The repo has a `.env` with two API keys and
AWS credentials, and a Terraform tree that has never been scanned. `gitleaks`
as a pre-commit hook and `checkov -d infra/terraform` in `make tf-check` are
cheap, and they are the module's own tools, so they are easy to name.

**6. Naming what loop.go is — Mód. 4-U2 (ReAct), Mód. 8-U2 (Tool-Using,
ReAct). built, unnamed.** `Loop.Run` is a ReAct loop: reason (text blocks),
act (tool_use), observe (tool_result), repeat, with `Halt` as the stop
condition. Say so in the file comment and in the syllabus entry; it is the
vocabulary an interviewer will use. Whether the loop stays is plan 5.1.

**7. Agentic RAG — Mód. 8-U4. no, and the reason is the best line in the
project.** Letting the generator fetch its own precedent was considered and
rejected: retrieval that only one of two judges can see breaks verification. It
is already in DECISIONS; it belongs in the syllabus artifact as an explicit
"no" with that sentence, because Agentic RAG is the fashionable answer and the
project has a structural argument against it.

## The rest of the syllabus, briefly

- **Mód. 1 (fundamentos, Web ML, Cursor, Ollama/OpenRouter).** Not this project,
  except one use for local models: developing graders and prompts against a
  local model to stop paying for iteration. Not for results; the numbers in the
  documents must come from the model that ships.
- **Mód. 2 (prompt chaining, templates, structured output, retries,
  observability).** Built: generator→verifier is the chain, `Render` is the
  template, forced tool use is the structured output, `anthropic.go` retries
  three times on 429/5xx. One thing to record: a retry after a client timeout
  can bill twice for a call that completed; acceptable, but say it.
- **Mód. 2 (semantic cache, response cache).** no. Every record is different
  and a cached letter for a *similar* dispute is the borrowing bug the precedent
  prompt exists to prevent. Prompt caching is the right cache here and it is
  built.
- **Mód. 3 (MCP).** Built, with the "what is left out" list. Missing per the
  module: authentication and service tokens (known), and a client in the
  course's language. A TypeScript MCP client test in `services/simulator` that
  calls the Go server over stdio would show the protocol crossing languages the
  same way `signing_test` does for HMAC. Small.
- **Mód. 4-U4 (memory: short, long, episodic).** no separate memory store, on
  purpose. `agent_runs` is the episodic memory and precedent is the long-term
  one, and both live in the record the verifier reads. A memory outside the
  record would be a fact the verifier has not seen.
- **Mód. 4-U5 (context pruning and stitching).** Built in miniature: precedent
  quotes truncated to 240 runes, tool results capped at 32 KB, lists at 50 rows.
  The claim itself has no cap (plan 3.3).
- **Mód. 4-U6, 4-U9, 8-U3 (LangGraph, CrewAI, multi-agent).** no, as the
  artifact says. One correction to its wording: by the syllabus' own taxonomy
  generator→verifier is a *Sequential* two-agent pattern. Say "sequential
  pipeline, not orchestrated multi-agent" rather than "not multi-agent".
- **Mód. 6-U2/U3/U4/U5/U8/U9 (IaC copilot, Kubernetes, AIOps, CI/CD, FinOps).**
  no; nothing is deployed. The transferable piece is U6/U11, and the review
  queue is exactly the approval-gate pattern.
- **Mód. 6-U6 (ChatOps com aprovação).** The review queue is this pattern with
  a web UI instead of Slack. A Slack bot would be a second front end for the
  same endpoint; not worth it.
- **Mód. 8-U4 (Confidence Thresholds, auto-approve).** after, in shadow mode,
  as the artifact says. The data to decide it is already recorded.
- **Mód. 8-U4 (Response Streaming).** no. Nobody waits on a draft; the queue is
  asynchronous by design.
- **Mód. 8-U5 (observability export: OTel, Langfuse).** after volume. The
  fingerprint fix (plan 4.5) is the cheap version that matters now.
- **Mód. 9 (fine-tuning).** no, as the artifact says. The honest addition: this
  system is *accumulating* the dataset a fine-tune would need (letter, verifier
  verdict, human decision, network outcome), which is the decision framework
  the module asks for. When there are thousands of rows with outcomes, the
  question reopens.
- **Mód. 10 (governança).** Explainability is `findings[].quote`; financial cost
  is the ceiling; the legal gap is PII in other cardholders' claims reaching
  prompts and an embedding provider (plan 3.4).
- **Mód. 5, 7, 11, 12.** UX tools, project management, the capstone and career.
  The capstone shape (Angular front, MCP enabled, RAG, an agent, a CLI to
  validate the logic, live demo and defence) is what this project already is;
  say that in one sentence rather than mapping it unit by unit.
