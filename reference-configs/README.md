# Reference EndpointPicker configurations

Complete `EndpointPickerConfig` files for the router policies that BLIS's router table offers as
nearest-policy targets, with an index (`policies.json`) that names each file, the knobs a
configuration search can sweep, and the router's own value for each knob. Every file is accepted
unchanged by the router's configuration loader, and a test routes requests through it.

## Contents

- [Who uses this](#who-uses-this)
- [The files](#the-files)
- [The llm-d guide files](#the-llm-d-guide-files)
- [What round-robin does](#what-round-robin-does)
- [Knobs](#knobs)
- [Reproduce the checks](#reproduce-the-checks)
- [What this does not do](#what-this-does-not-do)

## Who uses this

The accuracy dashboard and the configuration search read the configuration of a router from this
repository instead of writing one of their own, so that what BLIS runs is what the router parses
(design section 2.2, X1: router configuration reaches the router parser unchanged). A consumer
pins the commit of this repository that carries the index, reads `policies.json`, and passes the
named file to the router as its configuration, with a swept knob changed in a copy.

`policies.json` is the machine-readable entry point. Its `router` block states the release the
files are valid for: upstream `v1.0.0` (commit `b9393d1a9f1d5a72ecd86f8d2b643ab54c2a9b92`) plus the
fork's changes, tested from `4bce4f8f2176eb62ce648feb411d8f1ac19b0790`, the commit that hosts the plugins the guide files use. The files carry no commit of
their own; the commit that holds them is the pin, and the tests below fail at any commit where a
file stops loading or stops behaving as the index says.

## The files

"Verbatim" means a byte-for-byte copy of the router's file (a test shows it parses to the same
document); "composed" means no router sample states the configuration, so the file is assembled
from the pieces the router documents, and the sources are the pieces. Nothing in a file is a value
the router's sources do not state; where a knob has no value in a file, `policies.json` says
`not stated` and gives the router's default.

| Policy name | File | Origin | Sources |
|---|---|---|---|
| `default` | `default.yaml` | The router's built-in configuration, written as a file | `pkg/epp/config/loader/defaults.go:48-105`; used when no configuration is given, `pkg/epp/config/loader/configloader.go:80-84` |
| `round-robin` | `round-robin.yaml` | Composed | `pkg/epp/framework/plugins/scheduling/picker/maxscore/README.md:14,19`; `picker/maxscore/picker.go:88-130`; `picker/common.go:79-132` |
| `round-robin` (P/D) | `round-robin-pd.yaml` | Composed; profile pair and decider as the router's well-known `pd-disaggregation` configuration | `test/integration/epp/wellknown_configs_test.go:108-135`; `pkg/epp/framework/plugins/scheduling/profilehandler/disagg/README.md:149-160` |
| `random-picker` | `random-picker.yaml` | The frozen stable configuration of the random picker, without its defaulted parameters | `test/testdata/plugins/stable/v1/random-picker.yaml`; `pkg/epp/framework/plugins/scheduling/picker/random/picker.go:87-110` |
| `random-picker` (P/D) | `random-picker-pd.yaml` | Composed as `round-robin-pd.yaml` | as above |
| `least-active-requests` | `least-active-requests.yaml` | Composed | `pkg/epp/framework/plugins/scheduling/scorer/activerequest/README.md:3-11`; `active_request.go:99-113`; `cmd/epp/runner/runner.go:750` |
| `least-active-requests` (P/D) | `least-active-requests-pd.yaml` | Composed as `round-robin-pd.yaml` | as above |
| `pd-prefix-based` (P/D) | `pd-prefix-based.yaml` | Verbatim copy of the router's P/D sample | `deploy/config/pd-epp-config.yaml:1-37`; `profilehandler/disagg/README.md:90-123` |

The P/D files for `round-robin`, `random-picker` and `least-active-requests` use
`always-disagg-pd-decider`, so every request takes a prefill pick and a decode pick, as the
sensitivity runs of the router table did. `pd-prefix-based` uses the sample's
`prefix-based-pd-decider` and sends a request to a prefill endpoint only when its uncached prompt
reaches `nonCachedTokens`.

`default` has no P/D form: it has no prefill or decode profile and no role filter, so in a pool
that mixes roles it chooses among all endpoints and never names a prefill endpoint
(`TestDefaultHasNoPDForm`). A row of the router table that needs P/D routing under `default` has
no file here and must say so.

Not included: the router's simulation sample `deploy/config/sim-pd-epp-config.yaml`, which sets
`blockSizeTokens: 16` with `autoTune: false`; the producer raises a block size below 64 to 64 at
request time (`approximateprefix/plugin.go:48-56,170-176`), so the sample's setting is not what
runs.

## The llm-d guide files

`guides/` holds the router configuration of ten llm-d v1.0.0 guides
(`378b42d18dabe0b9c4ced4e6f3670d93b52db08e`), each a byte-for-byte copy of the plugin config in the
guide's Helm values file, dedented: the file a deployment of the guide runs. They are indexed in
`policies.json` like the others, with the guide path and line range in `copied_from` and `source`.
They are the files BLIS must run unedited (design X1); a copy that drops a plugin the host
cannot run, such as the screener, is not one of them.

| Policy | File | Source in the guide | Shape | Runs hosted from | Host assumption |
|---|---|---|---|---|---|
| `llm-d-optimized-baseline` | `guides/optimized-baseline.yaml` | `guides/optimized-baseline/router/optimized-baseline.values.yaml:15-30` | colocated | v1.0.0-blis.3 | none |
| `llm-d-pd-disaggregation` | `guides/pd-disaggregation.yaml` | `guides/pd-disaggregation/router/pd-disaggregation.values.yaml:17-59` | P/D | v1.0.0-blis.3 | none |
| `llm-d-tiered-prefix-cache-cpu` | `guides/tiered-prefix-cache-cpu.yaml` | `guides/tiered-prefix-cache/router/single-host/tiered-prefix-cache-cpu.values.yaml:15-52` | colocated | v1.0.0-blis.3 | none |
| `llm-d-nemotron-3-ultra` | `guides/nemotron-3-ultra.yaml` | `guides/models/nemotron-3-ultra/router/nemotron-3-ultra.values.yaml:25-54` | P/D | v1.0.0-blis.3 | none |
| `llm-d-precise-prefix-cache-routing` | `guides/precise-prefix-cache-routing.yaml` | `guides/precise-prefix-cache-routing/router/precise-prefix-cache-routing.values.yaml:43-140` | colocated | this change | render calls take no simulated time |
| `llm-d-wide-ep` | `guides/wide-ep.yaml` | `guides/wide-ep/router/wide-ep.values.yaml:15-50` | P/D | this change | one revision; no rollout is simulated |
| `llm-d-glm-5-2` | `guides/glm-5-2.yaml` | `guides/models/glm-5-2/router/glm-5-2.values.yaml:20-64` | P/D | this change | one revision; no rollout is simulated |
| `llm-d-glm-5-2-gpu-cpu-affinity` | `guides/glm-5-2-gpu-cpu-affinity.yaml` | `guides/models/glm-5-2/router/glm-5.2-overrides.values.yaml:9-63` | P/D | this change | one revision; no rollout is simulated |
| `llm-d-glm-5-2-precise` | `guides/glm-5-2-precise.yaml` | `guides/models/glm-5-2/router/precise-routing.values.yaml:20-100` | P/D | this change | one revision; no rollout is simulated; render calls take no simulated time |
| `llm-d-deepseek-v4` | `guides/deepseek-v4.yaml` | `guides/models/deepseek-v4/router/deepseek-v4.values.yaml:20-71` | P/D | this change | one revision; no rollout is simulated |

Fields an entry adds to the others:

- `requires`: what the scenario must provide. `epp_flags` (`--allow-experimental-plugins` for every
  file with the `disaggregatedset-rollout-screener`, which is Alpha; a test shows each file loads
  with the flag and, if the index says it is needed, is refused without it), `pod_labels` (the
  labels the screener reads: set name, revision, role; every pod of the pool must carry the
  revision label, and `SetEndpoint` refuses one that does not), `model` (the token-producer's model
  name, which the requests carry), `engine.block_size` and `kv_events` (the precise producer
  indexes the engines' KV events, and its block size must equal the engines'), `render_url`.
- `host_assumption`: what the host does not model and the file's behaviour rests on. The screener
  files: one revision, no rollout simulated; the screener then keeps every pod whose two required
  roles both have a Ready pod, and a role with no Ready pod drops every request, as in the router.
  The two files that render prompts on model servers: a render call takes no simulated time.
- `runs_hosted_from`: the fork tag from which the file runs hosted. The four that ran before this
  change say `v1.0.0-blis.3`; the other six need the plugins this change hosts, and say so in
  `needs`.
- `not_modelled`: the wide-EP and GLM guides list eight target ports (one per data-parallel rank).
  A pool with several target ports is a separate track; a hosted run of these files uses one port.
- `knobs_complete: false`: only parameters shown to move a hosted decision are listed. The other
  parameters of the plugins have no hosted test yet and are neither swept nor claimed ineffective.

To check a copy against the guide (from a checkout of llm-d at the tag, `G` its `guides/`
directory; the range and the eight-space indent are in the table and in `source`):

```sh
sed -n '15,50p' "$G/wide-ep/router/wide-ep.values.yaml" | sed 's/^        //' | diff - reference-configs/guides/wide-ep.yaml
```

## What round-robin does

The router at this commit has no policy that rotates strictly over endpoints, one per request.
`round-robin` is the max-score picker with no scorer: every candidate scores 0, and the picker
rotates the tied candidates by a counter (`picker/maxscore/picker.go:108-117`,
`picker/common.go:119-132`). The rotation is strict only if the candidates reach the picker in a
fixed order, and they do not:

- In a hosted runtime (the one BLIS embeds) the scheduler puts them in a seeded random order before
  every pick (`pkg/epp/scheduling/scheduler_profile.go:363-370`, added by the fork's hosting
  change). A rotation of a random order is a random order, so the choice is uniformly random, as
  `random-picker`'s is.
- Outside hosting the order is Go map order (`scheduler_profile.go:358-362`), which is not fixed
  either.

What the tests show, in a hosted router, for P/D pools of two, four and three endpoints each, 240
requests per pool (`TestRoundRobinOfAnEvenPDPoolReachesEveryEndpointButIsNotStrict`): every
endpoint of every pool is chosen, in the decode pool and the prefill pool alike, the counts are
close to equal (for example 111 and 129 in a pool of two, 52 to 65 in a pool of four), and the
sequence is not a fixed cycle. The counter is drawn at each profile pick, so a request that takes a
prefill pick and a decode pick draws it twice; with the candidates in a fixed order that would give
an even-sized pool only every other endpoint. Removing the shuffle shows it: with the line
commented out the same test finds one endpoint of two taking all 240 requests, and two endpoints of four taking 120 each.
The shuffle is what prevents it, and the test guards the shuffle.

So a substrate text that calls the policy round-robin and says it rotates is wrong in both
directions: it does not rotate, and it does not starve half an even-sized pool. It picks at random
among equal scores. `strict_per_request_rotation` is `false` for every policy in the index.

## Knobs

Each policy in `policies.json` has three lists.

- `knobs`: the parameters a configuration search may sweep. Each has `kind` (`scorer_weight` or
  `plugin_parameter`), the plugin, the value in the file or `not stated`, the router's default, the
  source line, and `hosted_effect: true`: moving it changes what the hosted router decides.
  `pkg/embed` `TestKnobMovesAHostedDecision` moves every knob in every file that carries it, over
  stub engines whose queue, KV usage and cache block size the test sets, and fails when the two
  settings decide alike. A few knobs say when they act (`effective_when`): `matchLengthScaleTokens`
  only while `matchLengthWeight` is above 0; `maxBusyScore` only at 0 in a file whose one scorer is
  the active-request scorer (any other value scales scores and keeps their order).
- `ineffective_parameters`: parameters the router accepts that cannot change a hosted decision,
  each with `hosted_effect: false` and a `reason`. A search must not sweep them.
  `TestIneffectiveParametersLeaveTheDecisionAlone` sets each one and shows the decisions do not
  move. They are `picker.maxNumOfEndpoints` (the hosted boundary keeps the first endpoint, in
  `pkg/embed/route.go`; the embed contract carries one endpoint per decision, so only the value 1
  is modelled), `inflight-load-producer.addEstimatedOutputTokens` (the active-request scorer reads
  request counts, not tokens), `prefix-based-pd-decider.prefixMatchInfoProducerName` (it names the
  one producer the file declares; any other name fails to load) and
  `approx-prefix-cache-producer.maxPrefixBlocksToMatch` (read only when `maxPrefixTokensToMatch` is
  0, and it is not).
- `pd_decider`, on the P/D files of `round-robin`, `random-picker` and `least-active-requests`: the
  decider is not a knob. Swapping `always-disagg-pd-decider` for `prefix-based-pd-decider` takes the
  producer, a prefix scorer in both profiles and a `nonCachedTokens` above 0, and replacing the
  name alone makes the file fail to load. `complete_variant` names the policy that is that whole
  file (`pd-prefix-based`); it scores with the prefix scorer 2 and the queue scorer 1, so it is a
  different policy, not this one with another decider.

A knob with `in_file: false` is set by adding the parameter (or the plugin, for the producers the
router creates itself) to a copy of the file. The tests check that the index agrees with the files
for every parameter stated in a file and that the file does not state the ones the index marks
`not stated`. The router defaults are transcribed from the cited lines; the tests assert only the
scorer-weight default and the picker default, which the loader exports.

The router's saturation detector drops an endpoint whose queue exceeds 5 or whose KV usage exceeds
0.8 before any scorer runs; a search that needs a scorer to decide must stay under those limits.

`maxPrefixTokensToMatch` in `pd-prefix-based` is 16384 in the sample and 131072 in the router
(`approximateprefix/types.go:131-135`); `lruCapacityPerServer` is 31250 in both.

## Reproduce the checks

From a checkout of this repository, with the Go toolchain `go.mod` names:

```sh
go test ./cmd/epp/runner/ -run 'ReferenceConfigsLoad|LoaderRefuses|PDSampleIsThe|BuiltInDefault|IndexKnobs|IndexSeparates|RoundRobinOutside'
go test ./pkg/embed/ -run 'RoundRobin|RandomPickerFiles|LeastActive|TestDefault|TestPDSample|TestKnobMoves|TestIneffective|TestPrefixMatchProducer|TestMaxNum|TestGuide'
```

Expected: `ok` for both, in about a second each. The first loads every indexed file through the
runner's own start-up path (phase one and two, experimental plugins refused unless the index says a file needs them), compares the
built-in default with `default.yaml` plugin for plugin and weight for weight, and checks the index
against the files. The second gives each file to the hosted router with a deterministic runtime
(`embedtest.NewRuntime(1)`) and routes requests: role filters hold, `least-active-requests` sends
a request to the endpoint with none in flight, `default` avoids a long queue and follows a cached
prefix, the sample disaggregates a new long prompt and not a cached or short one, and
raising `nonCachedTokens` above the prompt length stops it, and every knob in the index moves a
decision (and every ineffective parameter does not).

## What this does not do

- It changes no router behaviour: the diff is files, an index and tests.
- The `round-robin`, `random-picker` and `least-active-requests` files are compositions, not
  router samples; the router's maintainers have not named them. They are what the table's
  composition text describes.
- No file here claims to match Dynamo's behaviour. Whether a router-table row's nearest policy is
  near is the table's claim, measured in BLIS.
- The decision tests run the hosted router with a stub engine that reports an empty queue unless a
  test says otherwise. They do not measure latency or throughput.
- `pd-prefix-based` is checked for the decider's rule, not for the quality of its routing.
- No release or tag is made; the consumer pins a commit.
