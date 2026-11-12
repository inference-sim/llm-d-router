# Reference EndpointPicker configurations

Complete `EndpointPickerConfig` files for the router policies that BLIS's router table offers as
nearest-policy targets, with an index (`policies.json`) that names each file, the knobs a
configuration search can sweep, and the router's own value for each knob. Every file is accepted
unchanged by the router's configuration loader, and a test routes requests through it.

## Contents

- [Who uses this](#who-uses-this)
- [The files](#the-files)
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
fork's changes, tested from `dbe130dcc286510e390953bea7456a5aad10f076`. The files carry no commit of
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
go test ./pkg/embed/ -run 'RoundRobin|RandomPickerFiles|LeastActive|TestDefault|TestPDSample|TestKnobMoves|TestIneffective|TestPrefixMatchProducer|TestMaxNum'
```

Expected: `ok` for both, in about a second each. The first loads every indexed file through the
runner's own start-up path (phase one and two, experimental plugins refused), compares the
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
