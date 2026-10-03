// Package streamjson is the contract home for milk's batch JSONL transport —
// `milk "prompt" --output-format stream-json` (ADR-0050,
// docs/adr/0050-batch-stream-json-contract.md; the §6 catalog + §8.3
// conventions in docs/machine-readable-output-design.md).
//
// The locked wire artifacts live here: golden recordings of whole one-shot
// runs under testdata/events/ (with expected parse companions under
// testdata/expected/), validated by this package's contract tests against
// docs/schema/stream-json.schema.json. The §8.2 emission model (the typed
// event union in internal/events feeding this transport's encoder) lands per
// the design doc's §11 phasing; until then this package pins the *contract*
// so parallel implementation work — and external consumers — cannot drift the
// wire shape. Evolution is additive-only within capability `stream_v1`;
// shape changes require a superseding ADR (ADR-0050 rules 3–4).
package streamjson
