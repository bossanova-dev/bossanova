# Changelog

All notable changes to this project will be documented in this file.

## [1.6.1](https://github.com/recurser/bossanova/compare/v1.6.0...v1.6.1) (2026-04-23)

### Bug Fixes

* **bossd:** default daemon_id to hostname when BOSSD_DAEMON_ID is unset ([752941a](https://github.com/recurser/bossanova/commit/752941a7edc1730a462359883aa6ea31fb13c602))
* **tests:** unblock CI failures + stop keychain prompts ([9e47a6a](https://github.com/recurser/bossanova/commit/9e47a6a871504893699cdf54ab4ed6c364199334))
* **web:** persist session across refresh by forcing AuthKit devMode ([84a77e8](https://github.com/recurser/bossanova/commit/84a77e81e2bc9bbb5e7110b3cdc672ff85758ca7))

## [1.6.0](https://github.com/recurser/bossanova/compare/v1.5.0...v1.6.0) (2026-04-22)

### Features

* **boss,bossd:** [[#134](https://github.com/recurser/bossanova/issues/134)] format Linear issue block for PR body and prefill Claude ([6dab1b2](https://github.com/recurser/bossanova/commit/6dab1b2a2992465c2e382208759a3c7e58af0140))
* **boss:** [[#125](https://github.com/recurser/bossanova/issues/125)] add [t]erminal menu item to chat picker ([ba8f87d](https://github.com/recurser/bossanova/commit/ba8f87d031f2a4dbb54ef8c8f93c11cbffae9247))
* **boss:** [[#128](https://github.com/recurser/bossanova/issues/128)] add '/' filter to PR and Linear issue selectors ([71d5e5c](https://github.com/recurser/bossanova/commit/71d5e5c7c72a418220261d292d2f8d58b763bc0c))
* **boss:** [[#129](https://github.com/recurser/bossanova/issues/129)] add ctrl+b bug-report easter egg with Resend delivery ([75bbf62](https://github.com/recurser/bossanova/commit/75bbf62cc45bcdab02ecb385ce220f2d66fad0bb))
* **boss:** [[#129](https://github.com/recurser/bossanova/issues/129)] bake in production defaults for WorkOS, report URL, and orchestrator URL ([a848341](https://github.com/recurser/bossanova/commit/a848341513cadaa11d763340ce77a319d76adcb2))
* **boss:** [[#129](https://github.com/recurser/bossanova/issues/129)] simplify bug-report placeholder to "Describe the issue." ([75b1361](https://github.com/recurser/bossanova/commit/75b13610294944b74ce084a4208b2bfc236f4066))
* **boss:** [[#130](https://github.com/recurser/bossanova/issues/130)] add [m]erge action to chat picker for passing PRs ([fae8a25](https://github.com/recurser/bossanova/commit/fae8a2572228f5d969932a04a2616430b06ea9f2))
* **boss:** [[#131](https://github.com/recurser/bossanova/issues/131)] show spinner + optimistic status for PR merge ([efca0ab](https://github.com/recurser/bossanova/commit/efca0ab6e8bff0c1196d38482ed21808b19f5918))
* **bossd:** [[#120](https://github.com/recurser/bossanova/issues/120)] bound session poller with per-iteration timeout ([d5c683c](https://github.com/recurser/bossanova/commit/d5c683c1b067fe97656a57d1b7bd619c3b79d613))
* **bossd:** [[#120](https://github.com/recurser/bossanova/issues/120)] Leg 1 — bounded goroutine shutdown + plugin host hardening ([fd46b9c](https://github.com/recurser/bossanova/commit/fd46b9ce13d66d0da8a62272c883f9aabfa7b096))
* **bosso:** [[#120](https://github.com/recurser/bossanova/issues/120)] remaining P0s — NewID errors, CORS fail-closed, Litestream warning ([0e20c0e](https://github.com/recurser/bossanova/commit/0e20c0e612e9a10a00b108a589491cbb16056a12))
* **bosso:** [[#129](https://github.com/recurser/bossanova/issues/129)] harden bug-report flow with rate limiting and timeouts ([dba895e](https://github.com/recurser/bossanova/commit/dba895e3fdd361bea3caab44d072c984b9ce5233))
* **infra:** [[#129](https://github.com/recurser/bossanova/issues/129)] add root-zone SES inbound MX to Cloudflare module ([8fe0156](https://github.com/recurser/bossanova/commit/8fe01565f45b13d6437170bc4b49b577cc0930be))
* **infra:** [[#129](https://github.com/recurser/bossanova/issues/129)] wire Resend sender defaults and DNS records for bug reports ([24f543f](https://github.com/recurser/bossanova/commit/24f543fa61bf7b86d338bd8b46875dbb1f7b2bd8))
* **linear:** [[#132](https://github.com/recurser/bossanova/issues/132)] LINEAR_API_ENDPOINT env var override for E2E tests ([fe3eee9](https://github.com/recurser/bossanova/commit/fe3eee974b896cd7efc0a30baf930cf3ac05b58f))
* **linear:** [[#137](https://github.com/recurser/bossanova/issues/137)] push issue picker search down to the Linear API ([5634a7b](https://github.com/recurser/bossanova/commit/5634a7b6cd55297a24922c36f4ad8a4c79b607a3))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] add 30s timeout to daemon → plugin RPCs ([39531e3](https://github.com/recurser/bossanova/commit/39531e3e4b7607ed40a26df12c9c3c930f8fe5d3))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] add default RPC timeout to hostclient unary methods ([76b6ded](https://github.com/recurser/bossanova/commit/76b6dedf6b4b44eb389405cc6e786ce3358666fd))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] per-startup random plugin handshake cookie ([2d1cd4b](https://github.com/recurser/bossanova/commit/2d1cd4bd94c0b0f8d04809fe35ceb4bbf471c28f))
* **security:** [[#120](https://github.com/recurser/bossanova/issues/120)] per-install keyring passphrase + structured setup-script spec ([0d08695](https://github.com/recurser/bossanova/commit/0d08695b7c37990e5e9e3bfad146993a03d49eaf))

### Bug Fixes

* **autopilot:** [[#120](https://github.com/recurser/bossanova/issues/120)] make runWorkflow cancellable via Shutdown ([d630cc4](https://github.com/recurser/bossanova/commit/d630cc42e3b0428bbd0358f05df4f9be23a23769))
* **autopilot:** [[#122](https://github.com/recurser/bossanova/issues/122)] count bd tasks via --json to avoid false-positive pause ([32ba940](https://github.com/recurser/bossanova/commit/32ba940c3883a2dddb9fdc07fd33fabfa66b7881))
* **autopilot:** [[#123](https://github.com/recurser/bossanova/issues/123)] reject start when plan file is missing ([ba421cd](https://github.com/recurser/bossanova/commit/ba421cd8f0153bcb4a8429c88a7fe899fc54ad91))
* **boss:** [[#120](https://github.com/recurser/bossanova/issues/120)] close newsession stream on all terminal paths ([da37d02](https://github.com/recurser/bossanova/commit/da37d026ce0af20ddf5f5703795255fee0da80ca))
* **boss:** [[#125](https://github.com/recurser/bossanova/issues/125)] single-quote cwd in iTerm cd to block shell expansion ([e3b2afc](https://github.com/recurser/bossanova/commit/e3b2afc7c30a70362282a318779553ef7cd769fc))
* **boss:** [[#125](https://github.com/recurser/bossanova/issues/125)] use Ghostty --working-directory flag for new tab ([e08ed91](https://github.com/recurser/bossanova/commit/e08ed91bcd8b2eb533d7fd59bcda9758fa8f74bb))
* **boss:** [[#127](https://github.com/recurser/bossanova/issues/127)] open new Ghostty tab and hide action when unsupported ([0e113d3](https://github.com/recurser/bossanova/commit/0e113d3878936adaa87a860c54e92c7151842c8e))
* **boss:** [[#129](https://github.com/recurser/bossanova/issues/129)] restart tick chain after bug-report modal dismisses ([c9e336a](https://github.com/recurser/bossanova/commit/c9e336a6cbd31fe1a81674513f8ca68419f6cacb))
* **boss:** [[#129](https://github.com/recurser/bossanova/issues/129)] run go mod tidy for zerolog deps ([685d709](https://github.com/recurser/bossanova/commit/685d70996b2225e078f125c6fcdc5207d54f54fe))
* **boss:** [[#135](https://github.com/recurser/bossanova/issues/135)] return to session list and show merged status after boss merge ([5a77c39](https://github.com/recurser/bossanova/commit/5a77c39065e83bf9a766eebd5480b15fb0788c66))
* **boss:** [[#139](https://github.com/recurser/bossanova/issues/139)] don't mangle OSC 8 hyperlink in merged session titles ([c031688](https://github.com/recurser/bossanova/commit/c03168829370ef8d7a00391036358119d42beae5))
* **bossd:** [[#120](https://github.com/recurser/bossanova/issues/120)] deregister AttachSession subscriber on handler exit ([c1da83e](https://github.com/recurser/bossanova/commit/c1da83ebde3b7490f7b3390c330a4f0317225998))
* **bossd:** [[#124](https://github.com/recurser/bossanova/issues/124)] create draft PR for tracker-sourced sessions ([dfb8deb](https://github.com/recurser/bossanova/commit/dfb8deb2506cd3c2049a172d3e98c415ca5b9d34))
* **bossd:** [[#126](https://github.com/recurser/bossanova/issues/126)] push pending commits in SubmitPR when PR exists ([42ccc97](https://github.com/recurser/bossanova/commit/42ccc979d4f1b9b78614ef17bb62ae86d69d0ef7))
* **bossd:** [[#129](https://github.com/recurser/bossanova/issues/129)] check os.Setenv/Unsetenv errors to satisfy errcheck ([e5862ff](https://github.com/recurser/bossanova/commit/e5862ff11393a2fbbf94bba9e374ebdfeb684786))
* **bossd:** [[#132](https://github.com/recurser/bossanova/issues/132)] pass query arg to ListAvailableIssues in linear E2E tests ([3506e80](https://github.com/recurser/bossanova/commit/3506e8098022a36d9e7257055affc65991747889))
* **bossd:** [[#138](https://github.com/recurser/bossanova/issues/138)] sync local main repo around gh pr merge ([3ec9227](https://github.com/recurser/bossanova/commit/3ec92276943d491ceaaeeb9635262abb14fff024))
* **bossd:** [[#138](https://github.com/recurser/bossanova/issues/138)] sync the PR's actual base branch, not the repo default ([30551d1](https://github.com/recurser/bossanova/commit/30551d159b00062545973cf1160381d2c8224fb9))
* **bossd:** pass nil baseSyncer to taskorchestrator.New in dependabot test ([1246427](https://github.com/recurser/bossanova/commit/1246427e52db0fc6061c0867cf0dced83ee510ea))
* **ci:** [[#120](https://github.com/recurser/bossanova/issues/120)] add copy-skills prereq to bosso test/lint targets ([efa4cf8](https://github.com/recurser/bossanova/commit/efa4cf82629cc4f5c76859beffc76f229e706898))
* **ci:** [[#120](https://github.com/recurser/bossanova/issues/120)] golangci-lint v2 schema + action v7 polish ([39042f4](https://github.com/recurser/bossanova/commit/39042f4453c4e091ac6be821edc063f9b479fab0))
* **ci:** [bossa-nzr] exclude gen/ from golangci formatters ([6df4cd3](https://github.com/recurser/bossanova/commit/6df4cd381b63d3c264c36a0039013e0f7f74b91b))
* **ci:** deploy to Cloudflare Pages via pnpm dlx instead of wrangler-action ([0d3694f](https://github.com/recurser/bossanova/commit/0d3694f47090589bd9286e02bfe0bb30a90a9be2))
* **keyring:** [bossa-at5] persist passphrase outside tmpfs + atomic create ([217f360](https://github.com/recurser/bossanova/commit/217f360113521fcf6cd555fb58c569b68166b803))
* **newsession:** [[#137](https://github.com/recurser/bossanova/issues/137)] drop stale issue fetches by seq, not query ([16d5a21](https://github.com/recurser/bossanova/commit/16d5a219faa6cb7a3f694c02a82984c9d754b893))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] bound EagerClient.broker.Dial with 10s timeout ([7d3fd2f](https://github.com/recurser/bossanova/commit/7d3fd2f292316f95536ca35a9218f278bb4a9fc3))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] bound plugin Kill with SIGKILL fallback on timeout ([cf6f9c6](https://github.com/recurser/bossanova/commit/cf6f9c63ea4e3f42e811c83be686366006a4d0d2))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] emit "not meant to be executed" when cookie env is unset ([2f4ecf5](https://github.com/recurser/bossanova/commit/2f4ecf5540008fc02959c122b795734d34596ddf))
* **plugin:** [[#120](https://github.com/recurser/bossanova/issues/120)] validate HostService deps in Host.Start ([b9b7aac](https://github.com/recurser/bossanova/commit/b9b7aac8118f7b0a3bc1a0b28cd2d7ab430d9a83))
* **race:** [[#120](https://github.com/recurser/bossanova/issues/120)] eliminate three data races across safego, tuidriver, autopilot ([573c277](https://github.com/recurser/bossanova/commit/573c27774a8130a8ffc3e57dd55cd2c0588fbb8f))
* **repair:** [[#120](https://github.com/recurser/bossanova/issues/120)] re-read config under second lock to avoid stale cooldown ([020df4e](https://github.com/recurser/bossanova/commit/020df4eb0dd9d9aa38ecc7a9415e115ae934fdd5))
* **repair:** [[#120](https://github.com/recurser/bossanova/issues/120)] track repair goroutines on WaitGroup, drain on Shutdown ([93c9696](https://github.com/recurser/bossanova/commit/93c9696924437512bc56b3d43154b04aa932ad57))
* **repair:** [[#140](https://github.com/recurser/bossanova/issues/140)] enforce one active repair workflow per session ([a76e2c9](https://github.com/recurser/bossanova/commit/a76e2c9e06c610c7b67c367303d194a9a602767d))
* **repair:** [[#140](https://github.com/recurser/bossanova/issues/140)] skip cooldown when losing CreateWorkflow race ([6a7076b](https://github.com/recurser/bossanova/commit/6a7076b7866b773b9dbadf5f706b5badd15019ed))
* **review:** [[#128](https://github.com/recurser/bossanova/issues/128)] rebuild filtered indices on data refetch and consolidate Activate ([9a489b8](https://github.com/recurser/bossanova/commit/9a489b81beb737bd7d4a96007252f48191da3816))
* **review:** [[#128](https://github.com/recurser/bossanova/issues/128)] rebuild table on filter activation to fix stale height ([322a17d](https://github.com/recurser/bossanova/commit/322a17d1f3d4d078be957181bded6dd4e9dc1645))
* **review:** [[#128](https://github.com/recurser/bossanova/issues/128)] separate Activate() call from return to fix evaluation order ([bba750a](https://github.com/recurser/bossanova/commit/bba750a5014d0e85d4e669daf0a297428ee55e28))
* **review:** [[#129](https://github.com/recurser/bossanova/issues/129)] address Cursor Bugbot feedback on bug-report flow ([bed657a](https://github.com/recurser/bossanova/commit/bed657a449dc747751489aaa9173ead6fba93362))
* **review:** [[#129](https://github.com/recurser/bossanova/issues/129)] address second round of Cursor Bugbot feedback ([6984c54](https://github.com/recurser/bossanova/commit/6984c54b225ad6a0e7d7953a7b17215aed5d1069))
* **review:** [[#129](https://github.com/recurser/bossanova/issues/129)] HTML-escape r.ID in bug report email body ([596b9ed](https://github.com/recurser/bossanova/commit/596b9eda38bf7868a44f7f573a105d85267fa466))
* **review:** [[#129](https://github.com/recurser/bossanova/issues/129)] plumb daemon statuses into bug reports ([bdabf38](https://github.com/recurser/bossanova/commit/bdabf380177d74dd2d71153d33be2c03d4412a85))
* **review:** [[#131](https://github.com/recurser/bossanova/issues/131)] block key input while merge is in flight ([495de51](https://github.com/recurser/bossanova/commit/495de514a13fa72c811da89f7131157d9d8e3b8e))
* **review:** [[#131](https://github.com/recurser/bossanova/issues/131)] preserve optimistic MERGED status across refresh ([eb7b759](https://github.com/recurser/bossanova/commit/eb7b759bd228ef9a7987b21efbf434d2d17818bb))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] address Bugbot feedback on plugin harness + Linear test ([611e2df](https://github.com/recurser/bossanova/commit/611e2dfab85ca97ff00fb68c6c75377315b2d54c))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] clarify FAKE_CLAUDE_HANDOFF_DIR skip comment ([02ba1e2](https://github.com/recurser/bossanova/commit/02ba1e2697ed66ff274875c4d415b794230a6e29))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] clear LINEAR_API_ENDPOINT in endpoint-default test ([e362672](https://github.com/recurser/bossanova/commit/e3626729b5c050546e3955d093898123ebab42a0))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] fail loudly when autopilot harness caller sets FAKE_CLAUDE_HANDOFF_DIR ([c3f8ddf](https://github.com/recurser/bossanova/commit/c3f8ddfcfcf1e15e68d318dfb13e5738081aea39))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] gate LINEAR_API_ENDPOINT override behind e2e build tag ([fff8749](https://github.com/recurser/bossanova/commit/fff8749c48bf45a56bfab784d205f03c48e01777))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] guard AttachSession against closed channel ([d72c8bd](https://github.com/recurser/bossanova/commit/d72c8bdaeaee86b6adee359f88b9f54c559f441f))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] order t.Cleanup to tear down host before bus ([fa48f5b](https://github.com/recurser/bossanova/commit/fa48f5b0433f7547c8bd38d485eb0c1556836e81))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] remove dead `last` variable in waitForWorkflowStatus ([1e804ea](https://github.com/recurser/bossanova/commit/1e804eabee14e132f6df6ab936cd098e31091385))
* **review:** [[#132](https://github.com/recurser/bossanova/issues/132)] use runtime.Caller for workspaceRoot in plugin harness ([78d2377](https://github.com/recurser/bossanova/commit/78d23770fea8567effaf396fc51b4352bb31b5a8))
* **statusdetect:** [[#126](https://github.com/recurser/bossanova/issues/126)] ignore tool-output and empty input prompts ([1da7c8f](https://github.com/recurser/bossanova/commit/1da7c8f951119cec125911bea8dc33f705d22887))
* **statusdetect:** [[#136](https://github.com/recurser/bossanova/issues/136)] don't treat Claude Code "Tip:" lines as questions ([551fb6d](https://github.com/recurser/bossanova/commit/551fb6def65f45cf7f3047d535df80c6b354a246))
* **test:** [[#132](https://github.com/recurser/bossanova/issues/132)] kill fake_claude subprocesses before TempDir cleanup ([6ab145c](https://github.com/recurser/bossanova/commit/6ab145cdd2f358a73aa966247987c7491d6d3c98))
* **tuitest:** [[#132](https://github.com/recurser/bossanova/issues/132)] include pid in mock daemon socket path ([33aa1a9](https://github.com/recurser/bossanova/commit/33aa1a9063a6173ed157cafd61f82ede6f51d4ba))
* **web:** [[#120](https://github.com/recurser/bossanova/issues/120)] remove ApiContext casing collision and hardcoded devMode ([3428757](https://github.com/recurser/bossanova/commit/3428757bcda939b4e5a6574577aa45fed512c2b5))
* **web:** move @rolldown/binding-darwin-arm64 to optionalDependencies ([dabc65f](https://github.com/recurser/bossanova/commit/dabc65f0cdb075937ec9436b81e2f3f7c409a4ad))

### Performance Improvements

* **autopilot:** [[#132](https://github.com/recurser/bossanova/issues/132)] sub-second poll interval + immediate first check ([161c1e4](https://github.com/recurser/bossanova/commit/161c1e47e828283be2e69d2e594685e287f8a018))
* **db:** [[#120](https://github.com/recurser/bossanova/issues/120)] add missing indexes for repo_id / claude_id lookups ([cdec1bc](https://github.com/recurser/bossanova/commit/cdec1bc0d54513adfe1c19830955f2c5492387e1))
* **db:** [[#120](https://github.com/recurser/bossanova/issues/120)] eliminate N+1 queries and atomize session sync ([e5b5c54](https://github.com/recurser/bossanova/commit/e5b5c54c52ef92626bf6e477482b55c586e15505))
* **db:** [[#120](https://github.com/recurser/bossanova/issues/120)] raise SQLite pool to 8 conns with per-connection pragmas ([4aeb946](https://github.com/recurser/bossanova/commit/4aeb9463622619756b4f01ad98110e873c591c00))

## [1.5.0](https://github.com/recurser/bossanova/compare/v1.4.0...v1.5.0) (2026-04-20)

### Features

* **boss,bossd:** notify daemon of auth changes after login/logout ([1144432](https://github.com/recurser/bossanova/commit/1144432072105e3f89a1780ca4f47a93c230add6))
* **bossd:** [[#115](https://github.com/recurser/bossanova/issues/115)] add SessionLister interface to upstream ([0fc90f2](https://github.com/recurser/bossanova/commit/0fc90f2990ae0bb98ab01c9510feb802cadbed2a))
* **boss:** improve empty-state guidance on welcome and repo list ([6b5ae98](https://github.com/recurser/bossanova/commit/6b5ae9862e50270764e8a082544b91c62dd3a841))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] add SyncSessions RPC and expand sessions_registry schema ([720914f](https://github.com/recurser/bossanova/commit/720914fd0c28888b5daa231cfddcbd24c71b9aac))
* **boss:** refine no-sessions empty state copy ([ad52c42](https://github.com/recurser/bossanova/commit/ad52c4229586815f44971b4c567e05bb838b6d2e))

### Bug Fixes

* **bosso,bossd:** [[#115](https://github.com/recurser/bossanova/issues/115)] wrap UpsertBatch in transaction and cache repo lookups ([f4998fb](https://github.com/recurser/bossanova/commit/f4998fb1d5eb4c40f99d43e70c5ce3315c171e00))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] address gosec and staticcheck lint warnings ([9e42517](https://github.com/recurser/bossanova/commit/9e425170252295dc23b1a1e8472083fdeddc96b9))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] apply request filters in ProxyListSessions DB fallback ([cc1050e](https://github.com/recurser/bossanova/commit/cc1050e2ec65b3a2f7d5519e87261b8dfb6e75f4))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] fix DB fallback logic and UpsertBatch daemonID usage ([c285e19](https://github.com/recurser/bossanova/commit/c285e19f8842364a06c6123c34ecef8c2091c962))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] fix gofmt trailing newline ([b86b123](https://github.com/recurser/bossanova/commit/b86b1231bcc3ed80c5ca5439da28b6816ccf9d43))
* **bosso:** [[#115](https://github.com/recurser/bossanova/issues/115)] make TestProxyListSessions_FallsBackToDB order-independent ([b25e97e](https://github.com/recurser/bossanova/commit/b25e97e758ce18a7d149f4c2795ed97d69c4aac2))
* **repair:** [[#117](https://github.com/recurser/bossanova/issues/117)] unblock stuck implementing_plan sessions ([cba04a6](https://github.com/recurser/bossanova/commit/cba04a67534b3e81e799207df58bc029abb4edbd))
* **repair:** prevent force-advance from being blocked by stuck timeout ([d0cf752](https://github.com/recurser/bossanova/commit/d0cf752844678d8a050d06e4b813f92302e0695f))

## [1.4.0](https://github.com/recurser/bossanova/compare/v1.3.3...v1.4.0) (2026-04-17)

### Features

* **auth:** [[#110](https://github.com/recurser/bossanova/issues/110)] migrate from Auth0 to WorkOS across CLI, server, and web ([7ad29b2](https://github.com/recurser/bossanova/commit/7ad29b2ddb1be4ff0bfd0a3bb35bca37cd92f886))
* **infra:** add DNS CNAME record for Pages custom domain ([6a9f129](https://github.com/recurser/bossanova/commit/6a9f1292c6ad94cbadbf9a5bace00e2cbcbeb939))
* **infra:** add DNS CNAME record for Pages custom domain ([628dcc6](https://github.com/recurser/bossanova/commit/628dcc69c985b57057772065b356af37493e0c2e))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] per-chat tmux sessions and daemon-side status polling ([2fc8ede](https://github.com/recurser/bossanova/commit/2fc8ede002f134fe5318f8a9846190552702fcfa))

### Bug Fixes

* **auth:** [[#111](https://github.com/recurser/bossanova/issues/111)] add aud claim to test JWT helpers ([077f419](https://github.com/recurser/bossanova/commit/077f41961674f81d35b66f013c307588d441ecc1))
* **auth:** [[#111](https://github.com/recurser/bossanova/issues/111)] add JWT audience validation for 'bosso' ([ea0e291](https://github.com/recurser/bossanova/commit/ea0e2912ba4458f2081bb7a8e196667f6338aa40))
* **auth:** [[#112](https://github.com/recurser/bossanova/issues/112)] set keychain item label to fix repeating password prompt ([24f507d](https://github.com/recurser/bossanova/commit/24f507dc3c071f995035a9247bce782584597644))
* **autopilot:** pass prompt as CLI arg instead of stdin pipe in tmux ([7888f04](https://github.com/recurser/bossanova/commit/7888f0415c4c0a9559e7b548d7249d6ffef1e06e))
* **autopilot:** use stdin redirect from plan file instead of CLI arg ([398ff2f](https://github.com/recurser/bossanova/commit/398ff2ff71f13d1a1529dce436626172959296a6))
* **bosso:** jit-create user in auth middleware for web SPA access ([3dc1bff](https://github.com/recurser/bossanova/commit/3dc1bff768dd2b30d9373c7981bce12683e90d83))
* **bosso:** use array syntax for http_service.checks in fly.toml ([dd61d76](https://github.com/recurser/bossanova/commit/dd61d767664ec83e1746f396329aa4ad9931d8eb))
* **bosso:** use correct WorkOS User Management issuer for JWT validation ([6c6135d](https://github.com/recurser/bossanova/commit/6c6135d1f094cf23088bc8144fb3721880ae911e))
* **build:** [[#111](https://github.com/recurser/bossanova/issues/111)] add .gitkeep to skills/ so go:embed pattern resolves in fresh clones ([b7e6077](https://github.com/recurser/bossanova/commit/b7e607722aa76d9ed60bbd611565d09573cdf961))
* **ci:** [[#111](https://github.com/recurser/bossanova/issues/111)] use GITHUB_PATH instead of env PATH override for protoc-gen-es ([5b8d5cc](https://github.com/recurser/bossanova/commit/5b8d5cc745796821d16de32a76470900ab048107))
* **ci:** add .npmrc with strict-dep-builds=false for pnpm 10 ([096810c](https://github.com/recurser/bossanova/commit/096810c6ec9cbc7c39d794edaeb43b0f63196812))
* **ci:** add protobuf generation to deploy-web jobs ([784a296](https://github.com/recurser/bossanova/commit/784a2967af754255e6fa677c647e46908b179ae2))
* **ci:** add protobuf generation to deploy-web jobs ([f12324e](https://github.com/recurser/bossanova/commit/f12324e58f71ddb184bc2c36abfb9569013a507b))
* **ci:** approve pnpm build scripts to unblock staging deploy ([4de63c7](https://github.com/recurser/bossanova/commit/4de63c7058bc7f9303b1461f1347f6b86a2d1bdf))
* **ci:** correct semantic-release plugin version specifiers ([3bb878c](https://github.com/recurser/bossanova/commit/3bb878c98ca6a8eea91c7bbc6306fbf7f41e0dc0))
* **ci:** correct semantic-release plugin version specifiers ([6512632](https://github.com/recurser/bossanova/commit/6512632177d5ba6b5e2acb4ee568fc8c9174fa65))
* **ci:** move onlyBuiltDependencies to pnpm-workspace.yaml for pnpm 10 ([556c372](https://github.com/recurser/bossanova/commit/556c3729268b9335656002e7e9d9deacdd433934))
* **ci:** pass --config.strict-dep-builds=false directly to pnpm install ([ca10066](https://github.com/recurser/bossanova/commit/ca10066cf5dfeee4b7d8975803ae6c4c9056cde5))
* **ci:** pass --config.strict-dep-builds=false directly to pnpm install ([fee9833](https://github.com/recurser/bossanova/commit/fee983314ae76cf67fed05f17040e845aaa4ed97))
* **ci:** pass Cloudflare account ID to wrangler to skip memberships lookup ([1d2cd27](https://github.com/recurser/bossanova/commit/1d2cd27d9d70b6e88661ee0a66a97458a8f447d2))
* **ci:** remove bash parameter expansion from semantic-release exec cmd ([e0193aa](https://github.com/recurser/bossanova/commit/e0193aa066653ef6fd8d22c5754480d4ae33e781))
* **ci:** remove bash parameter expansion from semantic-release exec cmd ([bb87d1f](https://github.com/recurser/bossanova/commit/bb87d1fab0618d9a53c71a3d3ca2065db1e13c92))
* **ci:** strip v prefix from semantic-release version output ([939bebb](https://github.com/recurser/bossanova/commit/939bebbd16d77e0e957f80982501348b3a203056))
* **ci:** upgrade GitHub Actions to silence Node.js 20 deprecation warnings ([cb05568](https://github.com/recurser/bossanova/commit/cb055689ea1d3913aacf9c5b1daa81d047eaadc4))
* **ci:** upgrade pnpm from 9 to 10 to fix ERR_PNPM_IGNORED_BUILDS ([7aca163](https://github.com/recurser/bossanova/commit/7aca16337fcb05ba019a5bd2a7d82094bc95df05))
* **ci:** use GITHUB_PATH for protoc-gen-es and add plugin stubs to Dockerfile ([4aada25](https://github.com/recurser/bossanova/commit/4aada2537be89e84cb1ed0b662030b983a613633))
* **ci:** use GITHUB_PATH for protoc-gen-es instead of env PATH override ([960ba27](https://github.com/recurser/bossanova/commit/960ba276ce3b5c3a1e7d5a3b0f3023ad863bb965))
* **ci:** use ignoredBuiltDependencies for @bufbuild/buf postinstall ([ea06358](https://github.com/recurser/bossanova/commit/ea06358aab7facbda07025a7bd304067cdc0e9c2))
* **deploy:** [[#111](https://github.com/recurser/bossanova/issues/111)] wire Terraform + deploy config for bosso and web ([35f1dc0](https://github.com/recurser/bossanova/commit/35f1dc06603714cadc60e3ccb79ca8ecfb236ef5))
* **deploy:** [[#111](https://github.com/recurser/bossanova/issues/111)] wire Terraform Cloud + branch-triggered deploy pipeline ([e02c4ba](https://github.com/recurser/bossanova/commit/e02c4ba66da56c7ee6a88819c6989caac57f7e2e))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] remove Fly module from Terraform, manage with flyctl only ([0898031](https://github.com/recurser/bossanova/commit/08980318143431c756810c945d745d003120b706))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] set Fly primary region to ams (Amsterdam) ([9404f10](https://github.com/recurser/bossanova/commit/9404f1086edc0d7b7ad0eb0af8b1ad708db3857f))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] set R2 bucket location to WEUR (Western Europe) ([ab63efd](https://github.com/recurser/bossanova/commit/ab63efd40cddb2d61a1f643bcc3e7dcb4ad1760a))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use 'personal' as Fly.io org slug ([9b6638b](https://github.com/recurser/bossanova/commit/9b6638b3e5304a71dc565e8dcf2c9b4eee601c50))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use correct fly provider version constraint ~> 0.0.20 ([858e7d1](https://github.com/recurser/bossanova/commit/858e7d155038374393084026c482601921be254a))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use correct Fly.io org slug dave-perrett ([a17e626](https://github.com/recurser/bossanova/commit/a17e6264d47d3037b2405bd911e24fffce527928))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use separate WorkOS client IDs for staging and production ([7fc2693](https://github.com/recurser/bossanova/commit/7fc26938092fc89f7d3ac34b386f0a1d3a413359))
* **infra:** hardcode WorkOS CNAME target (cname.workosdns.com) ([00ae162](https://github.com/recurser/bossanova/commit/00ae1623bd82176035446f29cd0c24c33d08cf0f))
* **infra:** resolve merge conflict, keep clean main (no WorkOS auth domain) ([d1da11c](https://github.com/recurser/bossanova/commit/d1da11c1fa09e0c9d89d8a68918e2ad765059eca))
* **infra:** use DNS-only (no proxy) for Fly.io CNAME records ([1dc025a](https://github.com/recurser/bossanova/commit/1dc025a31ee92c940c2545dbd6328da1b66685a3))
* **orchestrator:** [[#108](https://github.com/recurser/bossanova/issues/108)] stop retrying failed task mappings to prevent duplicate sessions ([63df67a](https://github.com/recurser/bossanova/commit/63df67ab70c03f1ddc5f5169e38c8a76d4f138de))
* **repair:** [[#109](https://github.com/recurser/bossanova/issues/109)] skip repair when head commit SHA matches last attempt ([063f7a6](https://github.com/recurser/bossanova/commit/063f7a624c785232edab5b82a3ad3b5ccc164658))
* **review:** [[#111](https://github.com/recurser/bossanova/issues/111)] add middleware exemption tests and CF Pages preview config ([dfe9616](https://github.com/recurser/bossanova/commit/dfe961667bf688e019619c14a822153bdc9934f8))
* **status:** [[#108](https://github.com/recurser/bossanova/issues/108)] capture tmux scrollback to prevent false positive question detection ([55b2eae](https://github.com/recurser/bossanova/commit/55b2eae1cf2e18249b4a90826fd67ef8d2d0fb6f))
* **status:** [[#113](https://github.com/recurser/bossanova/issues/113)] sessions show idle instead of working on daemon startup ([eb6e511](https://github.com/recurser/bossanova/commit/eb6e5118d2b0730630dabe5acf1514b3877e8cc1))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] add csi-u key format and TERM_PROGRAM passthrough ([4b5f8f7](https://github.com/recurser/bossanova/commit/4b5f8f7594d6968d28b6277763e77c955d999e39))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] fix key bindings, extended-keys, and enable mouse scrolling ([b101f45](https://github.com/recurser/bossanova/commit/b101f45492817cee2b8338f75b989ec500088226))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] set mouse mode globally and add Shift+Enter test scaffolding ([87faede](https://github.com/recurser/bossanova/commit/87faede7e3d1d04f01a86bb13a6cd98d738462f7))
* **tui:** [[#106](https://github.com/recurser/bossanova/issues/106)] remove misleading "ready for review" attention alert ([82290fe](https://github.com/recurser/bossanova/commit/82290fe7a9af5d20100db1e657dbe06b93f883ea))
* **tui:** remove extra blank line below banner across all screens ([d0c0f00](https://github.com/recurser/bossanova/commit/d0c0f009d670528dc1ec2b994b6b9ea8178e3e0e))
* **tui:** remove extra blank line below header in create session screen ([cbf4d65](https://github.com/recurser/bossanova/commit/cbf4d6515df63ceeb2d76ccba99058c9bf99cd4f))
* **web:** [[#111](https://github.com/recurser/bossanova/issues/111)] migrate services/web from npm to pnpm workspace ([797ad1c](https://github.com/recurser/bossanova/commit/797ad1c0af57e431f9b42f2d32d4a0e2d95f6f85))
* **web:** add name field and remove build section from wrangler.toml ([4235ea9](https://github.com/recurser/bossanova/commit/4235ea9e2ac250dbcec499e9edef6b0c1a95a19b))
* **web:** enable AuthKit devMode for client-only token storage ([84c4d2a](https://github.com/recurser/bossanova/commit/84c4d2a911d4631e8f3680be9351d15480a28684))
* **web:** extract SessionsTable to fix Biome excessive-lines lint error ([6360b98](https://github.com/recurser/bossanova/commit/6360b9855965d76a5ef8286341b64003b929e978))
* **web:** harden AuthKit token storage by removing devMode ([c9c0790](https://github.com/recurser/bossanova/commit/c9c07900550babe247ef9b949a92948f0adf3480))
* **web:** hide nav links and suppress API calls when signed out ([44a5e0f](https://github.com/recurser/bossanova/commit/44a5e0fbc27fce0d5a9a9d9bd164e0a7a7efb13d))

### Reverts

* **web:** restore AuthKit devMode, remove custom auth domain infra ([538dd9f](https://github.com/recurser/bossanova/commit/538dd9fccf9b449edc8068840cbdfa8ba4a3d69f))

## [1.3.3](https://github.com/recurser/bossanova/compare/v1.3.2...v1.3.3) (2026-04-08)

### Bug Fixes

* **build:** handle empty skills directory in public repo ([64e8456](https://github.com/recurser/bossanova/commit/64e8456c72ceb106d340e7b7686082ffa33a5069))
* **homebrew:** remove post_install hook that fails in sandbox ([05d99dd](https://github.com/recurser/bossanova/commit/05d99ddd4bcbb8ded84af848abd98b3008cfb593))

## [1.3.2](https://github.com/recurser/bossanova/compare/v1.3.1...v1.3.2) (2026-04-08)

### Bug Fixes

* **mirror:** remove hardcoded plugin name references from public-visible code ([bbc0f64](https://github.com/recurser/bossanova/commit/bbc0f6479b82008096e445d78107e39490e3ec3e))
* **mirror:** remove private module references from public repo build ([84488fd](https://github.com/recurser/bossanova/commit/84488fd5fc8490e6568bfa6ad7277b848aac33a1))

## [1.3.1](https://github.com/recurser/bossanova/compare/v1.3.0...v1.3.1) (2026-04-08)

### Bug Fixes

* **mirror:** use read-tree to populate index during commit replay ([64185c1](https://github.com/recurser/bossanova/commit/64185c18cdc84afda32331c80f0f6a0276095cf0))

## [1.3.0](https://github.com/recurser/bossanova/compare/v1.2.0...v1.3.0) (2026-04-08)

### Features

* **mirror:** replay commits individually to build public history ([88268d0](https://github.com/recurser/bossanova/commit/88268d05503800b8b795319f2cbbd6e5d1932675))

### Bug Fixes

* **mirror:** preserve public repo history across releases ([c53070e](https://github.com/recurser/bossanova/commit/c53070e6d610952895981debccbe6d045458e081))
* **mirror:** use orphan commit to prevent private history leaking ([4397612](https://github.com/recurser/bossanova/commit/439761219e2e2ec8be65d4918367e11d1879b551))

## [1.2.0](https://github.com/recurser/bossanova/compare/v1.1.6...v1.2.0) (2026-04-08)

### Features

* **config:** auto-discover plugins relative to binary path ([361e252](https://github.com/recurser/bossanova/commit/361e25262a2b6e4bbe5b71207b9a4a347ebb1eee))
* **config:** fall back to same-dir plugin discovery for dev mode ([88d9ebe](https://github.com/recurser/bossanova/commit/88d9ebe66f9996f405c827da514a28f194f7a0a4))

### Bug Fixes

* **bossd:** persist auto-discovered plugins to settings on first run ([dfdd3f2](https://github.com/recurser/bossanova/commit/dfdd3f261f55f9e2db7634f09b0df34c44d41c8c))
* **global:** trigger a release ([610c656](https://github.com/recurser/bossanova/commit/610c656cb74a1fee533c2ffe93caea477f85bd7f))
* **homebrew:** set executable permission on plugin binaries ([5f30a2a](https://github.com/recurser/bossanova/commit/5f30a2a25d3e9ea5e7e86a104446764bf95bbcf0))

## [1.1.6](https://github.com/recurser/bossanova/compare/v1.1.5...v1.1.6) (2026-04-08)

### Bug Fixes

* **ci:** create Formula/ directory for fresh homebrew-tap repo ([9b45753](https://github.com/recurser/bossanova/commit/9b457537834713b984b41727f82c532b19a1155d))
* **ci:** mirror only production to public repo's main branch ([2f0fa69](https://github.com/recurser/bossanova/commit/2f0fa69e8177fdbc9df82c4f6966c457dbb556ce))
* **ci:** strip .claude and .husky from public mirror ([9d6827f](https://github.com/recurser/bossanova/commit/9d6827f69c9cdfdfff9391b642c75508d4b94f8b))
* **global:** trigger a release ([128869b](https://github.com/recurser/bossanova/commit/128869be5dfc1f07e570d82a8ac2d353883b423e))

## [1.1.5](https://github.com/recurser/bossanova/compare/v1.1.4...v1.1.5) (2026-04-08)

### Bug Fixes

* **ci:** add persist-credentials: false to release and homebrew checkouts ([9a56201](https://github.com/recurser/bossanova/commit/9a56201cd7196e1aa618bfe442a321f4dca2ab6b))
* **global:** trigger a release ([e8b3e76](https://github.com/recurser/bossanova/commit/e8b3e763293ec1715da6ea2b6025d9ab85c3c6d3))

## [1.1.4](https://github.com/recurser/bossanova/compare/v1.1.3...v1.1.4) (2026-04-08)

### Bug Fixes

* **global:** trigger a release ([1b68301](https://github.com/recurser/bossanova/commit/1b683017979847eaae50a308d6191bd5c22cc925))

## [1.1.3](https://github.com/recurser/bossanova/compare/v1.1.2...v1.1.3) (2026-04-08)

### Bug Fixes

* **ci:** use default token for checkout in release and homebrew jobs ([d8906a2](https://github.com/recurser/bossanova/commit/d8906a2cd3e1b4dfee7a6a2d77ce7ad0ec8b598e))
* **global:** trigger a release ([0097438](https://github.com/recurser/bossanova/commit/0097438eeaaac328e70e2efaab9646e5fb64fc53))

## [1.1.2](https://github.com/recurser/bossanova/compare/v1.1.1...v1.1.2) (2026-04-08)

### Bug Fixes

* **ci:** upgrade artifact actions to Node 24, fix Go cache, skip notarize gracefully ([8e52763](https://github.com/recurser/bossanova/commit/8e52763eea99656531ff171c3ca627c58457c170))
* **global:** trigger a release ([0f80f61](https://github.com/recurser/bossanova/commit/0f80f61eaa9bcaa406f905940f2ab1be3d6207cf))

## [1.1.1](https://github.com/recurser/bossanova/compare/v1.1.0...v1.1.1) (2026-04-08)

### Bug Fixes

* **ci:** upgrade artifact actions to Node 24, fix Go cache, skip notarize gracefully ([2bc9cdd](https://github.com/recurser/bossanova/commit/2bc9cddc1ef62655595d403dfa6a5abe92802cf1))

## [1.1.0](https://github.com/recurser/bossanova/compare/v1.0.1...v1.1.0) (2026-04-08)

### Features

* **ci:** add workflow_dispatch action to create production release PR ([d077253](https://github.com/recurser/bossanova/commit/d077253f7d858f1d3ac84d3f6eb040a01c08e8f4))
* **make:** add release target to trigger production release workflow ([d0b6e64](https://github.com/recurser/bossanova/commit/d0b6e64f5d485a9ba7884d4ac4ab10060af189ea))

### Bug Fixes

* **ci:** strip .beads/ from public mirror, remove deprecated split workflow ([ee6a3b3](https://github.com/recurser/bossanova/commit/ee6a3b3847ada773b554ab3a281d2ca0ff2095ee))
* **ci:** use per-module targets in main CI, strip infra/ from public mirror ([f79fdbe](https://github.com/recurser/bossanova/commit/f79fdbe5fd8b6343c32847f07875977fda7b4954))
* **global:** fix fetch-depth during mirroring ([a31a1ab](https://github.com/recurser/bossanova/commit/a31a1ab47e6196cf1864fb78bd29ba2e4ce8dbc8))
* **global:** rename the mirror-public action's GITHUB_TOKEN env-var to avoid collisions ([3661e52](https://github.com/recurser/bossanova/commit/3661e5222c32e94cd1367d72ad58164f52556838))

## [1.0.1](https://github.com/recurser/bossanova/compare/v1.0.0...v1.0.1) (2026-04-08)

### Bug Fixes

* **global:** rename the mirror-public action's GITHUB_TOKEN env-var to avoid collisions ([2188f12](https://github.com/recurser/bossanova/commit/2188f1238d9f9b16704ae183077317519655e6cd))

## 1.0.0 (2026-04-08)

## [1.0.0-staging.19](https://github.com/recurser/bossanova/compare/v1.0.0-staging.18...v1.0.0-staging.19) (2026-04-17)

### Bug Fixes

* **ci:** pass --config.strict-dep-builds=false directly to pnpm install ([fee9833](https://github.com/recurser/bossanova/commit/fee983314ae76cf67fed05f17040e845aaa4ed97))
* **infra:** resolve merge conflict, keep clean main (no WorkOS auth domain) ([d1da11c](https://github.com/recurser/bossanova/commit/d1da11c1fa09e0c9d89d8a68918e2ad765059eca))

### Reverts

* **web:** restore AuthKit devMode, remove custom auth domain infra ([538dd9f](https://github.com/recurser/bossanova/commit/538dd9fccf9b449edc8068840cbdfa8ba4a3d69f))

## [1.0.0-staging.18](https://github.com/recurser/bossanova/compare/v1.0.0-staging.17...v1.0.0-staging.18) (2026-04-17)

### Bug Fixes

* **infra:** hardcode WorkOS CNAME target (cname.workosdns.com) ([00ae162](https://github.com/recurser/bossanova/commit/00ae1623bd82176035446f29cd0c24c33d08cf0f))

## [1.0.0-staging.17](https://github.com/recurser/bossanova/compare/v1.0.0-staging.16...v1.0.0-staging.17) (2026-04-17)

### Bug Fixes

* **ci:** pass --config.strict-dep-builds=false directly to pnpm install ([ca10066](https://github.com/recurser/bossanova/commit/ca10066cf5dfeee4b7d8975803ae6c4c9056cde5))

## [1.0.0-staging.16](https://github.com/recurser/bossanova/compare/v1.0.0-staging.15...v1.0.0-staging.16) (2026-04-17)

### Bug Fixes

* **ci:** add .npmrc with strict-dep-builds=false for pnpm 10 ([096810c](https://github.com/recurser/bossanova/commit/096810c6ec9cbc7c39d794edaeb43b0f63196812))

## [1.0.0-staging.15](https://github.com/recurser/bossanova/compare/v1.0.0-staging.14...v1.0.0-staging.15) (2026-04-17)

### Bug Fixes

* **ci:** use ignoredBuiltDependencies for @bufbuild/buf postinstall ([ea06358](https://github.com/recurser/bossanova/commit/ea06358aab7facbda07025a7bd304067cdc0e9c2))

## [1.0.0-staging.14](https://github.com/recurser/bossanova/compare/v1.0.0-staging.13...v1.0.0-staging.14) (2026-04-17)

### Bug Fixes

* **ci:** move onlyBuiltDependencies to pnpm-workspace.yaml for pnpm 10 ([556c372](https://github.com/recurser/bossanova/commit/556c3729268b9335656002e7e9d9deacdd433934))

## [1.0.0-staging.13](https://github.com/recurser/bossanova/compare/v1.0.0-staging.12...v1.0.0-staging.13) (2026-04-17)

### Bug Fixes

* **ci:** upgrade pnpm from 9 to 10 to fix ERR_PNPM_IGNORED_BUILDS ([7aca163](https://github.com/recurser/bossanova/commit/7aca16337fcb05ba019a5bd2a7d82094bc95df05))

## [1.0.0-staging.12](https://github.com/recurser/bossanova/compare/v1.0.0-staging.11...v1.0.0-staging.12) (2026-04-17)

### Bug Fixes

* **web:** harden AuthKit token storage by removing devMode ([c9c0790](https://github.com/recurser/bossanova/commit/c9c07900550babe247ef9b949a92948f0adf3480))

## [1.0.0-staging.11](https://github.com/recurser/bossanova/compare/v1.0.0-staging.10...v1.0.0-staging.11) (2026-04-17)

### Bug Fixes

* **ci:** approve pnpm build scripts to unblock staging deploy ([4de63c7](https://github.com/recurser/bossanova/commit/4de63c7058bc7f9303b1461f1347f6b86a2d1bdf))

## [1.0.0-staging.10](https://github.com/recurser/bossanova/compare/v1.0.0-staging.9...v1.0.0-staging.10) (2026-04-17)

### Features

* **infra:** add DNS CNAME record for Pages custom domain ([6a9f129](https://github.com/recurser/bossanova/commit/6a9f1292c6ad94cbadbf9a5bace00e2cbcbeb939))
* **infra:** add DNS CNAME record for Pages custom domain ([628dcc6](https://github.com/recurser/bossanova/commit/628dcc69c985b57057772065b356af37493e0c2e))

### Bug Fixes

* **ci:** upgrade GitHub Actions to silence Node.js 20 deprecation warnings ([cb05568](https://github.com/recurser/bossanova/commit/cb055689ea1d3913aacf9c5b1daa81d047eaadc4))
* **infra:** use DNS-only (no proxy) for Fly.io CNAME records ([1dc025a](https://github.com/recurser/bossanova/commit/1dc025a31ee92c940c2545dbd6328da1b66685a3))
* **web:** enable AuthKit devMode for client-only token storage ([84c4d2a](https://github.com/recurser/bossanova/commit/84c4d2a911d4631e8f3680be9351d15480a28684))

## [1.0.0-staging.9](https://github.com/recurser/bossanova/compare/v1.0.0-staging.8...v1.0.0-staging.9) (2026-04-17)

### Bug Fixes

* **bosso:** use array syntax for http_service.checks in fly.toml ([dd61d76](https://github.com/recurser/bossanova/commit/dd61d767664ec83e1746f396329aa4ad9931d8eb))
* **ci:** pass Cloudflare account ID to wrangler to skip memberships lookup ([1d2cd27](https://github.com/recurser/bossanova/commit/1d2cd27d9d70b6e88661ee0a66a97458a8f447d2))

## [1.0.0-staging.8](https://github.com/recurser/bossanova/compare/v1.0.0-staging.7...v1.0.0-staging.8) (2026-04-17)

### Bug Fixes

* **web:** add name field and remove build section from wrangler.toml ([4235ea9](https://github.com/recurser/bossanova/commit/4235ea9e2ac250dbcec499e9edef6b0c1a95a19b))

## [1.0.0-staging.7](https://github.com/recurser/bossanova/compare/v1.0.0-staging.6...v1.0.0-staging.7) (2026-04-17)

### Bug Fixes

* **ci:** use GITHUB_PATH for protoc-gen-es and add plugin stubs to Dockerfile ([4aada25](https://github.com/recurser/bossanova/commit/4aada2537be89e84cb1ed0b662030b983a613633))
* **ci:** use GITHUB_PATH for protoc-gen-es instead of env PATH override ([960ba27](https://github.com/recurser/bossanova/commit/960ba276ce3b5c3a1e7d5a3b0f3023ad863bb965))

## [1.0.0-staging.6](https://github.com/recurser/bossanova/compare/v1.0.0-staging.5...v1.0.0-staging.6) (2026-04-17)

### Bug Fixes

* **ci:** add protobuf generation to deploy-web jobs ([784a296](https://github.com/recurser/bossanova/commit/784a2967af754255e6fa677c647e46908b179ae2))

## [1.0.0-staging.5](https://github.com/recurser/bossanova/compare/v1.0.0-staging.4...v1.0.0-staging.5) (2026-04-17)

### Bug Fixes

* **ci:** add protobuf generation to deploy-web jobs ([f12324e](https://github.com/recurser/bossanova/commit/f12324e58f71ddb184bc2c36abfb9569013a507b))

## [1.0.0-staging.4](https://github.com/recurser/bossanova/compare/v1.0.0-staging.3...v1.0.0-staging.4) (2026-04-17)

### Bug Fixes

* **ci:** remove bash parameter expansion from semantic-release exec cmd ([e0193aa](https://github.com/recurser/bossanova/commit/e0193aa066653ef6fd8d22c5754480d4ae33e781))

## [1.0.0-staging.3](https://github.com/recurser/bossanova/compare/v1.0.0-staging.2...v1.0.0-staging.3) (2026-04-17)

### Bug Fixes

* **ci:** remove bash parameter expansion from semantic-release exec cmd ([bb87d1f](https://github.com/recurser/bossanova/commit/bb87d1fab0618d9a53c71a3d3ca2065db1e13c92))

## [1.0.0-staging.2](https://github.com/recurser/bossanova/compare/v1.0.0-staging.1...v1.0.0-staging.2) (2026-04-17)

### Bug Fixes

* **ci:** correct semantic-release plugin version specifiers ([3bb878c](https://github.com/recurser/bossanova/commit/3bb878c98ca6a8eea91c7bbc6306fbf7f41e0dc0))

## 1.0.0-staging.1 (2026-04-17)

### Features

* add Bubbletea app shell with daemon client wiring ([87fd411](https://github.com/recurser/bossanova/commit/87fd4114c3b938725f32d9956ea2c7b3a6c8c6b6))
* add chat history picker for resuming Claude Code conversations ([cbb839a](https://github.com/recurser/bossanova/commit/cbb839ac6c08f96ceaef40f61546a89db54d9926))
* add CI check poller for sessions in AwaitingChecks state ([52ed4fb](https://github.com/recurser/bossanova/commit/52ed4fb81bb080fa4acf3cae3667286290702416))
* add ClaudeRunner interface and process manager with ring buffer ([7cb4d84](https://github.com/recurser/bossanova/commit/7cb4d842e08c86ca3946ae717b2f27b990de8408))
* add CloneAndRegisterRepo RPC and repo-add source-select wizard ([e24b866](https://github.com/recurser/bossanova/commit/e24b866d5cf609d3b004f309367dd467658500c2))
* add Cobra command structure with all CLI subcommands ([775b685](https://github.com/recurser/bossanova/commit/775b68500eac8bc751e33db74af7d5c92883afcf))
* add ConnectRPC server scaffold with Unix socket listener ([4ad6856](https://github.com/recurser/bossanova/commit/4ad68561c7503a167d083155b94d57a2430248a0))
* add cross-platform build system and version commands ([e693b52](https://github.com/recurser/bossanova/commit/e693b52f164551eb36942419825bb1dcbcb76c9f))
* add domain types and proto conversion functions ([c99821c](https://github.com/recurser/bossanova/commit/c99821c685ad919549197c77666454ffe168d018))
* add E2E test harness with mock git, Claude, and VCS ([62736e0](https://github.com/recurser/bossanova/commit/62736e0fcf93d5b7e295a10d80fb9708c4c21525))
* add event dispatcher routing VCS events to state machine transitions ([b6de38d](https://github.com/recurser/bossanova/commit/b6de38d3089869b92a7df24589b078985a10fe10))
* add fix loop handlers for check failure, conflict, and review feedback ([8d13181](https://github.com/recurser/bossanova/commit/8d1318138d0a883150f9f635311d9b408968f7ac))
* add GitHub provider with core VCS methods (CreateDraftPR, GetPRStatus, GetCheckResults) ([0dec8e4](https://github.com/recurser/bossanova/commit/0dec8e496a0ff6183bf5248d9c9fe603d86f2051))
* add Go multi-module scaffold + protobuf definitions (flight leg 1) ([a9cecc9](https://github.com/recurser/bossanova/commit/a9cecc95901d14c564c337a3a3b4a4b1ee3fa52d))
* add Homebrew formula template and generator for boss + bossd ([06ab1da](https://github.com/recurser/bossanova/commit/06ab1da010b2bf80423526dc4f191c291bcd2881))
* add initial SQLite migration for repos, sessions, and attempts ([444a071](https://github.com/recurser/bossanova/commit/444a0712062132edbd9abdd07181cc896463e9e5))
* add input validation to ListRepoPRs RPC stub ([4ee19da](https://github.com/recurser/bossanova/commit/4ee19da8cfacb9f2302155a246cefae97a5a3180))
* add panic recovery to all goroutines in bossd and bosso ([7598eaf](https://github.com/recurser/bossanova/commit/7598eaf86be96a961ce88f0a4bac1b2c0ddbe628))
* add Push method to WorktreeManager interface and implementation ([aa0f62f](https://github.com/recurser/bossanova/commit/aa0f62f50b7119e1ade1b5915892cd1cb1a6bde7))
* add SessionLifecycle orchestrator wiring worktree + claude + state machine ([9a99e29](https://github.com/recurser/bossanova/commit/9a99e2907dda63fde602ef172c751af9d2836645))
* add shared migration runner using goose in lib/bossalib/migrate ([b3dcec5](https://github.com/recurser/bossanova/commit/b3dcec58ab53b0b1eef2f7da3342983fabfa943b))
* add SQLite module with WAL mode and FK enforcement for bossd ([38b9603](https://github.com/recurser/bossanova/commit/38b9603a353cf5aeb1c819696e00bb0c5c16f2a1))
* add store interfaces and SQLite implementations for repos, sessions, attempts ([b5e79b4](https://github.com/recurser/bossanova/commit/b5e79b4e6474a8f815bf010d34c7178c937ed688))
* add styled home screen with session table, state colors, and polling ([552686e](https://github.com/recurser/bossanova/commit/552686ed02e64ec9899bcb44f9e525519bdb376c))
* add SubmitPR lifecycle method (push, create draft PR, transition to AwaitingChecks) ([a2f3c5c](https://github.com/recurser/bossanova/commit/a2f3c5ccc5b98f418de16cdc7a10443edfcf9a25))
* add VCS-agnostic interfaces, types, and events ([96620e7](https://github.com/recurser/bossanova/commit/96620e7b819e26322365f0f51a7d2491feb5a591))
* add WorktreeManager interface and Create implementation ([7cdf2c6](https://github.com/recurser/bossanova/commit/7cdf2c69971e90a6182b681b82cdc621b71d8655))
* **auth:** [[#110](https://github.com/recurser/bossanova/issues/110)] migrate from Auth0 to WorkOS across CLI, server, and web ([7ad29b2](https://github.com/recurser/bossanova/commit/7ad29b2ddb1be4ff0bfd0a3bb35bca37cd92f886))
* **autopilot:** [[#22](https://github.com/recurser/bossanova/issues/22)] add handoff recovery step when no handoff file found ([7614643](https://github.com/recurser/bossanova/commit/761464341595c8dc5c7f060f47f50b813e165f18))
* **autopilot:** [[#26](https://github.com/recurser/bossanova/issues/26)] add session ID tracking to claude runner ([f10b88a](https://github.com/recurser/bossanova/commit/f10b88ac44d131d1b58f75cc3c75710ce7cfd00a))
* **autopilot:** [[#26](https://github.com/recurser/bossanova/issues/26)] register autopilot chats in session chat list ([f37a050](https://github.com/recurser/bossanova/commit/f37a0508fe93eb77fa461f071d7fc3cb8945ddb7))
* **boss,bossd:** [[#52](https://github.com/recurser/bossanova/issues/52)] add quick chat session mode without worktree/branch/PR ([6152328](https://github.com/recurser/bossanova/commit/61523284080dcfce0bc3944802c5966c3fb711c4))
* **boss,bossd:** [[#53](https://github.com/recurser/bossanova/issues/53)] stream setup script output during session creation ([d8804bf](https://github.com/recurser/bossanova/commit/d8804bf7c1687074d888bdbc03647fb28541d37e))
* **boss,bossd:** [[#58](https://github.com/recurser/bossanova/issues/58)] add delete-all option to session trash view ([12424d1](https://github.com/recurser/bossanova/commit/12424d10446d51f86e44cdc5d4384ba8890d2143))
* **boss,bossd:** [[#62](https://github.com/recurser/bossanova/issues/62)] allow sessions to be renamed ([4694c6c](https://github.com/recurser/bossanova/commit/4694c6ce2ba812f9a917d5b287f8d992a1a0c655))
* **boss:** [[#10](https://github.com/recurser/bossanova/issues/10)] display header/logo banner on every TUI screen ([85a419b](https://github.com/recurser/bossanova/commit/85a419baefa7d816af22fcf5ad4fe55f87e7ab41))
* **boss:** [[#11](https://github.com/recurser/bossanova/issues/11)] add per-repo settings screen with rename and automation toggles ([5a60d6e](https://github.com/recurser/bossanova/commit/5a60d6e2e7cfb407f977cc6c907fa6d71071aec0))
* **boss:** [[#12](https://github.com/recurser/bossanova/issues/12)] status polling, table selectors, TUI refinements ([d852973](https://github.com/recurser/bossanova/commit/d852973444eda9a9c2b32c2fd5bf5148cd046dac))
* **boss:** [[#13](https://github.com/recurser/bossanova/issues/13)] dynamic banner, GitHub NWO names, branch/worktree cleanup ([7657e7a](https://github.com/recurser/bossanova/commit/7657e7a464d40e6abfbc7c408ba82b09cdeeab82))
* **boss:** [[#16](https://github.com/recurser/bossanova/issues/16)] add session settings with rename and PR title sync ([2dc8215](https://github.com/recurser/bossanova/commit/2dc82150baf4af91d1df322e3716bf73f346fd16))
* **boss:** [[#17](https://github.com/recurser/bossanova/issues/17)] add question status for Claude Code prompts ([36e4c5e](https://github.com/recurser/bossanova/commit/36e4c5ed9375870d84ed1f49e2e824dc408351ee))
* **boss:** [[#24](https://github.com/recurser/bossanova/issues/24)] add setup command to repo settings form ([a087636](https://github.com/recurser/bossanova/commit/a087636ea1a0fb1f1d26a0c1b154e4058526799c))
* **boss:** [[#4](https://github.com/recurser/bossanova/issues/4)] add PTY-based session detach/reattach with status tracking ([6a86bc0](https://github.com/recurser/bossanova/commit/6a86bc0dd5eb8cb708a6908395f617f871635b19))
* **boss:** [[#41](https://github.com/recurser/bossanova/issues/41)] replace huh.Select PR picker with table selector ([7cc31a4](https://github.com/recurser/bossanova/commit/7cc31a4ab0301cae43ae77dd61772ff9d5199f42))
* **boss:** [[#44](https://github.com/recurser/bossanova/issues/44)] add approved PR display status with green styling ([9bea0c8](https://github.com/recurser/bossanova/commit/9bea0c826e7d1e6ef11b6b71fdd309bb4e2aedf7))
* **boss:** [[#44](https://github.com/recurser/bossanova/issues/44)] rename PR display reviewed→rejected with danger styling ([2595824](https://github.com/recurser/bossanova/commit/259582434c8d749f0250f36a3f8277d804e2056a))
* **boss:** [[#48](https://github.com/recurser/bossanova/issues/48)] show loading spinner for rejected status while checks run ([e30039e](https://github.com/recurser/bossanova/commit/e30039e7b9ca57371dcd43eddaac11821d257ee7))
* **boss:** [[#5](https://github.com/recurser/bossanova/issues/5)] add archive/trash management to session list TUI ([25db40b](https://github.com/recurser/bossanova/commit/25db40bb334f42e103ad4778c2518e85be36179a))
* **boss:** [[#6](https://github.com/recurser/bossanova/issues/6)] add global settings with dangerously-skip-permissions flag ([a99c361](https://github.com/recurser/bossanova/commit/a99c3618dc82eaf39b0c9ae5bc0b744e22ebc998))
* **boss:** [[#63](https://github.com/recurser/bossanova/issues/63)] add Ctrl-X as detach keybinding ([734d3a6](https://github.com/recurser/bossanova/commit/734d3a64ddc6dbd8d68c851d7968148d3f39ba29))
* **boss:** [[#64](https://github.com/recurser/bossanova/issues/64)] add /boss CLI reference skill with coverage test ([caf1ac2](https://github.com/recurser/bossanova/commit/caf1ac2baa9c457bce09bb7c95c2009c0dc93532))
* **boss:** [[#65](https://github.com/recurser/bossanova/issues/65)] add /tui-qa skill for agent-driven TUI quality assurance ([eec64ea](https://github.com/recurser/bossanova/commit/eec64eae3810c6e21604dc30fb014484b0ada022))
* **boss:** [[#65](https://github.com/recurser/bossanova/issues/65)] add TUI driver infrastructure for agent-driven testing ([d1acdff](https://github.com/recurser/bossanova/commit/d1acdff4de4576011f11859551f7095fa513eed3))
* **boss:** [[#9](https://github.com/recurser/bossanova/issues/9)] share chat status between boss clients via daemon heartbeats ([f2f2bbf](https://github.com/recurser/bossanova/commit/f2f2bbf5a7c48def34872c8780bcb14021e8c951))
* **boss:** add --remote flag and client factory for orchestrator mode ([188c8cb](https://github.com/recurser/bossanova/commit/188c8cb2b6fbe665dba3ec9b20233f9b741ec1a3))
* **boss:** add cross-instance session/chat status via daemon heartbeats ([ab99b5b](https://github.com/recurser/bossanova/commit/ab99b5b8f0728ee2d9db1bb3c3e3066ce4c5b1aa))
* **boss:** add daemon install/uninstall/status subcommands ([afb92a6](https://github.com/recurser/bossanova/commit/afb92a64bd342382d48019f1fec7e2f445a4c205))
* **boss:** add daemon-backed chat tracking and title extraction ([c27ef40](https://github.com/recurser/bossanova/commit/c27ef40d197aa7d9a7a78068cc5fb24fe1003aac))
* **boss:** add DeleteChat RPC and clean up orphan chat records ([bc6ba15](https://github.com/recurser/bossanova/commit/bc6ba154065f5de86eaee7d4733c1abb68f567e7))
* **boss:** add macOS LaunchAgent plist template for bossd ([04098b6](https://github.com/recurser/bossanova/commit/04098b6b320431963503aa2561391eb325316368))
* **boss:** add RemoteClient for orchestrator proxy communication ([57c0b20](https://github.com/recurser/bossanova/commit/57c0b207163682e97a6e9b2bb32b9497cdef4e8e))
* **boss:** auto-start daemon on first CLI invocation ([2a52a18](https://github.com/recurser/bossanova/commit/2a52a1840aed430c273e4f5f1ab7ac9c2d72c512))
* **bossd,boss:** [[#56](https://github.com/recurser/bossanova/issues/56)] display repairing status in TUI while repair is in progress ([d8f97cf](https://github.com/recurser/bossanova/commit/d8f97cf040fbd897d55e8be2bc0ca854c4e4455e))
* **bossd:** [[#12](https://github.com/recurser/bossanova/issues/12)] add PRTracker, DisplayPoller, poll interval config ([9e915d2](https://github.com/recurser/bossanova/commit/9e915d2bab52294fad7b258eee8d3c4985f24b00))
* **bossd:** [[#15](https://github.com/recurser/bossanova/issues/15)] add attention status with automation flags and TUI indicator ([9431afb](https://github.com/recurser/bossanova/commit/9431afbe348f8a0f86fa651b11175cf1af5fd292))
* **bossd:** [[#15](https://github.com/recurser/bossanova/issues/15)] add dependabot PR detection and auto-merge ([3810b09](https://github.com/recurser/bossanova/commit/3810b09a0a1f9ee7d94974bb07d132291a50192c))
* **bossd:** [[#18](https://github.com/recurser/bossanova/issues/18)] add task mapping store, HostService server, and plugin host enhancements ([bb7c6dd](https://github.com/recurser/bossanova/commit/bb7c6dda0a52554101d7e44bc7fd8b9396d56377))
* **bossd:** [[#31](https://github.com/recurser/bossanova/issues/31)] inject boss skills into ~/.claude/skills/bossanova/ ([e4f7b44](https://github.com/recurser/bossanova/commit/e4f7b4415d6ddcecece4ec79580e57b26c2091f2))
* **bossd:** [[#44](https://github.com/recurser/bossanova/issues/44)] promote bot reviewer comments to changes_requested ([3386a40](https://github.com/recurser/bossanova/commit/3386a40f85059f958342c3c05af2463c0af0e931))
* **bossd:** add claude_chats persistence layer ([64e1684](https://github.com/recurser/bossanova/commit/64e16849cc3cf4170e84e28ace839b40c115e66f))
* **bossd:** add upstream manager for orchestrator connection ([a945275](https://github.com/recurser/bossanova/commit/a94527524c1338f8a3cb9a4dca7f31d3dc87b339))
* **bossd:** create draft PR immediately for no-plan PR sessions ([288cd73](https://github.com/recurser/bossanova/commit/288cd7396efcdf58bcc92e5ab5a60ab13f7c1cb2))
* **bossd:** extend worktree manager for repo validation and existing branches ([cdff744](https://github.com/recurser/bossanova/commit/cdff744f5ecfa57598093c5496ba2a56a0de5283))
* **bossd:** implement ValidateRepoPath, Claude chat, and PR session RPCs ([6b2d749](https://github.com/recurser/bossanova/commit/6b2d749c877dc07f6127316533f7a097cb230b4f))
* **boss:** display autopilot workflow status in session list ([82ee96e](https://github.com/recurser/bossanova/commit/82ee96eec4168feea113529dc0fae6198f866898))
* **bossd:** update session lifecycle for PR branch checkout ([d506d58](https://github.com/recurser/bossanova/commit/d506d58e2b41cd9d75084d7e292b88b61883cfed))
* **bossd:** wire upstream manager into daemon entrypoint ([153d18c](https://github.com/recurser/bossanova/commit/153d18c232f1a0da206b06603f80ffe8d1432815))
* **boss:** implement boss login/logout with Auth0 PKCE flow and OS keychain ([568890b](https://github.com/recurser/bossanova/commit/568890b9e44abfe47102114c78ad4558a9fd4ba8))
* **bosso:** add container infrastructure for production deployment ([f073cf7](https://github.com/recurser/bossanova/commit/f073cf7d3f62c2fdf6749ecd0af30d52c2e49fd8))
* **bosso:** add CORS middleware for SPA cross-origin requests ([f699c77](https://github.com/recurser/bossanova/commit/f699c772a4777414f8ac3f9274ca55e59e8ec995))
* **bosso:** add orchestrator SQLite schema (users, daemons, sessions_registry, audit_log) ([1472a55](https://github.com/recurser/bossanova/commit/1472a550273cbb3e02b23a03491b41ebe588ebcb))
* **bosso:** add ProxyAttachSession streaming relay and Stop/Pause/Resume handlers ([060a485](https://github.com/recurser/bossanova/commit/060a485c5b2c1ab299e92f38123d24b78c22af5a))
* **bosso:** add ProxyListSessions and ProxyGetSession handlers with relay pool ([bfc947e](https://github.com/recurser/bossanova/commit/bfc947e4add5bd9793a6885cb7ab098ecd9d9df8))
* **bosso:** add webhook HTTP handler with VCS event routing ([d4d635e](https://github.com/recurser/bossanova/commit/d4d635ee43fefc11c7ef1934eef9058cb3fe8074))
* **bosso:** add webhook parser interface and GitHub implementation ([3ee1018](https://github.com/recurser/bossanova/commit/3ee101867361875f7b92e9cfa70e1bd86c4448b5))
* **bosso:** add webhook_configs migration and WebhookConfigStore ([189eab4](https://github.com/recurser/bossanova/commit/189eab46afe4fda403427c86a7407c52fd206883))
* **bosso:** implement daemon registry RPCs (RegisterDaemon, Heartbeat, ListDaemons) ([e955065](https://github.com/recurser/bossanova/commit/e955065392fca96105129da4cc99c3cb546b5fd0))
* **bosso:** implement JWT validation middleware for ConnectRPC ([25c5795](https://github.com/recurser/bossanova/commit/25c57959562b0f0c8221d5ce469a910f1ab48aea))
* **bosso:** implement orchestrator DB stores (UserStore, DaemonStore, SessionRegistryStore, AuditStore) ([1b7dcf4](https://github.com/recurser/bossanova/commit/1b7dcf4bae2129bc1c8cac5689a90f5107e769ea))
* **bosso:** implement orchestrator entry point with ConnectRPC server ([72f09e9](https://github.com/recurser/bossanova/commit/72f09e958f4d861899c6a6ef091885c6181a66d2))
* **bosso:** implement TransferSession handler for cross-daemon session moves ([f1b1afe](https://github.com/recurser/bossanova/commit/f1b1afea751d0ceb53c3097d2612bb1e8478bd9d))
* **bosso:** scaffold orchestrator module with DB package and migrations embed ([32968c3](https://github.com/recurser/bossanova/commit/32968c366175bca452cde26edf9340e59c10f9a2))
* **bosso:** wire webhook handler and config RPCs into orchestrator ([c5b85dc](https://github.com/recurser/bossanova/commit/c5b85dcf0b121496d6fd301fd6902b8e60bc24dc))
* **boss:** redesign TUI with dynamic layouts and daemon-backed chat picker ([e4d4dec](https://github.com/recurser/bossanova/commit/e4d4dec27f695970db189f9c36415270e042c3b7))
* **boss:** show session state instead of "stopped" in status column ([16d5700](https://github.com/recurser/bossanova/commit/16d57005c7d590aeae0fa64254d8718aaa2e7175))
* **boss:** smart session enter, history shortcut, and detach routing ([707cbd3](https://github.com/recurser/bossanova/commit/707cbd3f21e154ccdec976e03d54a18fddabcbe8))
* **build:** [[#6](https://github.com/recurser/bossanova/issues/6)] add services/Makefile aggregator ([683436c](https://github.com/recurser/bossanova/commit/683436c60ddb8c3e6e4b94b78414e5a2b6d6cc7c))
* **chatpicker:** [[#80](https://github.com/recurser/bossanova/issues/80)] add CREATED column and sort by creation time ([81a38c0](https://github.com/recurser/bossanova/commit/81a38c00208e4a909ed5a314fa32eee7e8fb2969))
* **ci:** add tag-triggered deploy workflow (deploy.yml) ([08a064a](https://github.com/recurser/bossanova/commit/08a064aa580108ca0d81e0654cfb5e8434483df7))
* **ci:** add web SPA CI workflow (test-web.yml) ([fd6fa19](https://github.com/recurser/bossanova/commit/fd6fa19c86252a0cfbfd7ca860d06d6b92743a51))
* **ci:** add workflow_dispatch action to create production release PR ([d077253](https://github.com/recurser/bossanova/commit/d077253f7d858f1d3ac84d3f6eb040a01c08e8f4))
* **cli+tui:** [[#27](https://github.com/recurser/bossanova/issues/27)] unify boss CLI and TUI functional parity ([9747bec](https://github.com/recurser/bossanova/commit/9747becf66368bd23341a4a8472dc9ba5b615a17))
* **cli:** add AttachView for live session output streaming ([ee29dda](https://github.com/recurser/bossanova/commit/ee29ddabd58ea7ef9795181d55a4bf82c209d3b2))
* **cli:** add entry point with argument parsing and route resolution ([5918c78](https://github.com/recurser/bossanova/commit/5918c7817bbe41525eaa001d9a76ef09fd325e4a))
* **cli:** add guided New Session wizard with repo select, mode, and plan input ([7ab9b14](https://github.com/recurser/bossanova/commit/7ab9b148f492ebb2acd7cbf9c4f9b0bbc7d57551))
* **cli:** add interactive home screen with session list and action bar ([60f84a8](https://github.com/recurser/bossanova/commit/60f84a87233084100ae23cf67c8715c19b71cdd5))
* **cli:** add repo management views (AddRepo wizard, RepoList, RepoRemove) ([3c6067c](https://github.com/recurser/bossanova/commit/3c6067cb4bf6cb35eeb755227c016c26f34bccf7))
* **cli:** add tsyringe DI container with IpcClient, Config, and Logger tokens ([10f5ab2](https://github.com/recurser/bossanova/commit/10f5ab2326e09f92bd46b6a4c8552c541845ff4c))
* complete GitHub provider (GetFailedCheckLogs, MarkReadyForReview, GetReviewComments, ListOpenPRs) ([6fcb63f](https://github.com/recurser/bossanova/commit/6fcb63feeb612df5d5d26c5f018314334ae32edf))
* **config:** auto-discover plugins relative to binary path ([361e252](https://github.com/recurser/bossanova/commit/361e25262a2b6e4bbe5b71207b9a4a347ebb1eee))
* **config:** fall back to same-dir plugin discovery for dev mode ([88d9ebe](https://github.com/recurser/bossanova/commit/88d9ebe66f9996f405c827da514a28f194f7a0a4))
* **core:** [[#90](https://github.com/recurser/bossanova/issues/90)] add proto, DB, daemon, and client for Linear integration ([fedd355](https://github.com/recurser/bossanova/commit/fedd355133a62b45daf2c8217e7b11860985a737))
* create ConnectRPC client for daemon over Unix socket ([2c5c7ca](https://github.com/recurser/bossanova/commit/2c5c7ca85d0348784eac08394b5137fc62a40334))
* **daemon:** add AttemptStore and set up ~/ path aliases ([a15500a](https://github.com/recurser/bossanova/commit/a15500af0af17a63c924dad47484ce38ef9d0d08))
* **daemon:** add IPC server, RPC dispatcher, and context resolution ([a57ff60](https://github.com/recurser/bossanova/commit/a57ff60f2cc94276e2d04c66563da8eec589ce1c))
* **daemon:** add RepoStore and SessionStore with CRUD operations ([d3f4ef8](https://github.com/recurser/bossanova/commit/d3f4ef827a6424d29412aed6917d383bf4a18277))
* **daemon:** add SQLite database service with versioned migration runner ([b529ee6](https://github.com/recurser/bossanova/commit/b529ee649a0b7bcf631f2804420d8b7fbd81960d))
* **daemon:** add tsyringe DI container with service tokens ([be899ed](https://github.com/recurser/bossanova/commit/be899edcabf15b128dab3920a9fab8d9d4148dc4))
* **daemon:** implement Claude session launcher using Agent SDK query() ([a0b4558](https://github.com/recurser/bossanova/commit/a0b4558fbe4e5324bcffc19ebc134002bc37b610))
* **daemon:** implement ClaudeSupervisor class (start/stop/pause/resume) ([bcab2eb](https://github.com/recurser/bossanova/commit/bcab2ebc1e5131e5aad51fc40365a94e9fd6cf67))
* **daemon:** implement git utility functions ([c2e0ede](https://github.com/recurser/bossanova/commit/c2e0edec1b8ef493ab4920211a00158ab5cd1439))
* **daemon:** implement GitHub API client via gh CLI ([2139915](https://github.com/recurser/bossanova/commit/2139915010c2c05855e411b6237b43c3ab875ab8))
* **daemon:** implement PR lifecycle management (push → draft PR → awaiting_checks) ([2fd6a77](https://github.com/recurser/bossanova/commit/2fd6a77e553dc091d89eeb51e91e6e90c6eddb22))
* **daemon:** implement PR state polling (60s interval for pollable sessions) ([70f1eaf](https://github.com/recurser/bossanova/commit/70f1eaf021170a7d1842d6abe3ece405ea0c568b))
* **daemon:** implement ready-for-review transition and PR merge handling ([c449515](https://github.com/recurser/bossanova/commit/c449515f5c623092c187e0295699e228dc48d86a))
* **daemon:** implement session output log capture ([9ecc7a8](https://github.com/recurser/bossanova/commit/9ecc7a80f0470c3b3385c13ffb1951e6f6b47a52))
* **daemon:** implement worktree cleanup and branch push ([949af49](https://github.com/recurser/bossanova/commit/949af49cf2c0640d8fee20fe8dc38a6468d9cc0d))
* **daemon:** implement worktree creation with setup script support ([335e43b](https://github.com/recurser/bossanova/commit/335e43bc664a80e17a7930bd35001e288c4a2a17))
* **daemon:** wire Claude session into lifecycle and IPC handlers ([9947078](https://github.com/recurser/bossanova/commit/9947078b712f2019d04e9a828c6797dc7ed5da71))
* **daemon:** wire PR automation into session lifecycle ([77d739d](https://github.com/recurser/bossanova/commit/77d739d1818f047af4996d0b748cd013baba6e89))
* **daemon:** wire worktree into session creation/removal lifecycle ([6766850](https://github.com/recurser/bossanova/commit/6766850df61c8c1bae913f1bf6df34d6294d43e0))
* detect origin URL on repo registration and add WorktreeManager to Server ([1242474](https://github.com/recurser/bossanova/commit/12424748186ab3c29abd1cc8c3d50c9778b92893))
* **distribution:** [[#61](https://github.com/recurser/bossanova/issues/61)] implement distribution infrastructure for first external user ([baba2c9](https://github.com/recurser/bossanova/commit/baba2c9fdc1ae03ec93ee9fd6f34024b7a7a178c))
* **git:** [[#93](https://github.com/recurser/bossanova/issues/93)] inject BOSS_REPO_DIR and BOSS_WORKTREE_DIR env vars into setup scripts ([9b70c33](https://github.com/recurser/bossanova/commit/9b70c339cf006a235a09cffddbf8fda386290cd4))
* handle branch-already-exists on worktree creation ([690b056](https://github.com/recurser/bossanova/commit/690b0563a56e24b2eb8c3949a7dfbfc69cf9d7ff))
* implement archive, resurrect, and trash empty commands ([1c480ac](https://github.com/recurser/bossanova/commit/1c480accc8e3316ba9ea2c70970ead235a48f2cf))
* implement Archive, Resurrect, EmptyTrash, and DetectOriginURL ([4b7dbca](https://github.com/recurser/bossanova/commit/4b7dbca50a3d04be954f9b7118ab5c4435546097))
* implement attach view with server-streaming output and detach ([62fdc9a](https://github.com/recurser/bossanova/commit/62fdc9ae7b6e42510e12848af6975de9ebef3af8))
* implement AttachSession streaming RPC stub with session validation ([195896f](https://github.com/recurser/bossanova/commit/195896faa6f54345ad3de7401b206a204a0beabc))
* implement boss ls non-interactive session listing ([8933df8](https://github.com/recurser/bossanova/commit/8933df8bfda637af04bdd0b41daf153414a48a46))
* implement new session wizard with multi-step TUI flow ([43ec454](https://github.com/recurser/bossanova/commit/43ec4548e2df0ac0d538d451656e3df06b51efde))
* implement repo management views (add wizard, list, remove) ([99e55cb](https://github.com/recurser/bossanova/commit/99e55cb45332d7374631f24c1b83042dfce851db))
* implement repo RPCs (RegisterRepo, ListRepos, RemoveRepo) ([e64df9d](https://github.com/recurser/bossanova/commit/e64df9d8ecc142f91444ff394211779b1caa3464))
* implement ResolveContext RPC with directory-based detection ([3747c38](https://github.com/recurser/bossanova/commit/3747c3836e009cbe70bcf3a61f4b1d52d3d08a37))
* implement session RPCs wired to SessionStore ([5e1ffed](https://github.com/recurser/bossanova/commit/5e1ffedf7fc314673b10e0e99fd76240b995a15b))
* implement session state machine with qmuntal/stateless ([bb44c0e](https://github.com/recurser/bossanova/commit/bb44c0e3c37abfc3e235c412a281e2642857d7ec))
* **infra:** add CF Pages project to cloudflare Terraform module ([7a4c188](https://github.com/recurser/bossanova/commit/7a4c18899e5565d8b393e037e774752b17277775))
* **infra:** consolidate Terraform envs into workspace-keyed config ([1c7792e](https://github.com/recurser/bossanova/commit/1c7792ec43c24b06be13504ea556825a25879ae0))
* **lib:** add ClaudeChat domain model and IsGitHubURL VCS helper ([b8f342a](https://github.com/recurser/bossanova/commit/b8f342aa89e59f5803f33e7b51179c7cd2e2f092))
* **linear:** [[#90](https://github.com/recurser/bossanova/issues/90)] add tracker_id/tracker_url to sessions with hyperlinked issue IDs ([0455e3b](https://github.com/recurser/bossanova/commit/0455e3b68f09a9a34e5028f9a585a2f115f9d050))
* **make:** add release target to trigger production release workflow ([d0b6e64](https://github.com/recurser/bossanova/commit/d0b6e64f5d485a9ba7884d4ac4ab10060af189ea))
* **mirror:** replay commits individually to build public history ([88268d0](https://github.com/recurser/bossanova/commit/88268d05503800b8b795319f2cbbd6e5d1932675))
* **mutate:** [[#68](https://github.com/recurser/bossanova/issues/68)] add mutation testing infrastructure via gremlins ([240ea6f](https://github.com/recurser/bossanova/commit/240ea6f635dc026208837954fa5c4bbecf68138f))
* **orchestrator:** [[#18](https://github.com/recurser/bossanova/issues/18)] add task orchestrator with routing, queuing, and daemon wiring ([111f763](https://github.com/recurser/bossanova/commit/111f76388bf49365865b40db6af7cca937707d3a))
* **orchestrator:** retry failed task mappings on re-poll ([7855e1c](https://github.com/recurser/bossanova/commit/7855e1c7040f80e4b8c8b46d0bb43ccb5d884f19))
* **plugin:** [[#15](https://github.com/recurser/bossanova/issues/15)] add event bus, plugin host, and GRPCPlugin bridges ([8f96aa8](https://github.com/recurser/bossanova/commit/8f96aa8b02c17a89eecd18d01e228fc74ed7e42d))
* **plugin:** [[#15](https://github.com/recurser/bossanova/issues/15)] wire plugin host into bossd startup/shutdown ([2786354](https://github.com/recurser/bossanova/commit/2786354df5b40124cd4951dab017f79e42aa258f))
* **plugin:** [[#18](https://github.com/recurser/bossanova/issues/18)] add bossd-plugin-dependabot binary with PR classification ([30ab315](https://github.com/recurser/bossanova/commit/30ab315b0c3b426120463c8d0c78a13366584f43))
* **plugin:** [[#18](https://github.com/recurser/bossanova/issues/18)] wire previously-rejected PR detection into PollTasks ([b889553](https://github.com/recurser/bossanova/commit/b8895531493264b4715e9bbd033c02f6b0184679))
* **plugin:** [[#90](https://github.com/recurser/bossanova/issues/90)] implement Linear TaskSource plugin with GraphQL client and tests ([3ee5254](https://github.com/recurser/bossanova/commit/3ee52544f856e0952d60f2940805fec6336a377f))
* **proto:** [[#15](https://github.com/recurser/bossanova/issues/15)] add plugin.proto with TaskSource, EventSource, Scheduler services ([8e95ecd](https://github.com/recurser/bossanova/commit/8e95ecd49decce978fa40f752413c5df1b89c0b0))
* **proto:** [[#18](https://github.com/recurser/bossanova/issues/18)] add TaskAction enum, HostService proto, and shared plugin library ([492a8b9](https://github.com/recurser/bossanova/commit/492a8b9b419727ed90a2d0241f83a5ce17d28646))
* **proto:** [[#22](https://github.com/recurser/bossanova/issues/22)] add autopilot workflow proto definitions and design doc ([ae99a46](https://github.com/recurser/bossanova/commit/ae99a46239fd1513d0729d769c764a563d025824))
* **proto:** add ValidateRepoPath, Claude chat tracking, and session extensions ([14eeb46](https://github.com/recurser/bossanova/commit/14eeb46ae993c37db137ca97cd1a099ae7ccba7c))
* **proto:** add webhook config RPCs and DeliverVCSEvent to daemon ([91464e5](https://github.com/recurser/bossanova/commit/91464e5aa03dafd2cd67ff66cb87befe4ff16e64))
* **repair:** [[#45](https://github.com/recurser/bossanova/issues/45)] add auto-repair plugin with session state guard ([f2ff8c2](https://github.com/recurser/bossanova/commit/f2ff8c2f22ae4ef47c292ac77c6ad4201049e80c))
* **repair:** [[#71](https://github.com/recurser/bossanova/issues/71)] include session name in repair log entries ([bb99282](https://github.com/recurser/bossanova/commit/bb992828ebab27c47adfdd77fcc3d8c39b7fb3f5))
* **repair:** [[#72](https://github.com/recurser/bossanova/issues/72)] advance stuck ImplementingPlan sessions in repair sweep ([73225c4](https://github.com/recurser/bossanova/commit/73225c4ac74506c9890eb72584a33757b836d985))
* scaffold monorepo with pnpm workspaces and 5 packages ([a63f9ab](https://github.com/recurser/bossanova/commit/a63f9abbd1b6dc61f12b062b61ffb03f39681a71))
* **server:** [[#22](https://github.com/recurser/bossanova/issues/22)] add daemon autopilot RPCs and CLI commands ([665244d](https://github.com/recurser/bossanova/commit/665244d0830339fda3be5c45118adc3bf459520d))
* **session:** [[#25](https://github.com/recurser/bossanova/issues/25)] skip setup script for dependabot PRs ([e56e8ac](https://github.com/recurser/bossanova/commit/e56e8ace8867cd6db03d4ae604844acffdf319ef))
* **settings:** [[#18](https://github.com/recurser/bossanova/issues/18)] add per-repo merge strategy configuration ([9ada5d8](https://github.com/recurser/bossanova/commit/9ada5d8adc6fc159a6f246efc7bc63ea9869f796))
* **shared:** add domain types, DB row types, and versioned migrations ([673140c](https://github.com/recurser/bossanova/commit/673140c3fd4e661fbb760310afdb22b28947551c))
* **shared:** add IPC client for CLI-to-daemon communication ([548204f](https://github.com/recurser/bossanova/commit/548204fe140612ffaa83c3af98c9985621b51a4e))
* **shared:** add JSON-RPC 2.0 schema for CLI-daemon IPC ([633c82e](https://github.com/recurser/bossanova/commit/633c82e71c95594bfb461ae0bda06b2e6936a7f3))
* **shared:** add webhook and daemon event type definitions ([4693041](https://github.com/recurser/bossanova/commit/46930414057d57a71543d26c756f1092a7a56aab))
* **shared:** add WebSocket protocol and barrel exports ([6c854b5](https://github.com/recurser/bossanova/commit/6c854b59248e23cc011f16c70acd5a738778d994))
* **shared:** add XState v5 session state machine ([aee3dd9](https://github.com/recurser/bossanova/commit/aee3dd9cb9e353844ec204f72b98f43e71619813))
* **skills:** add /post-flight-checks skill and unify testing across flight workflow ([3a5cc2b](https://github.com/recurser/bossanova/commit/3a5cc2be39eeb534657a73c5accdde9613598569))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] per-chat tmux sessions and daemon-side status polling ([2fc8ede](https://github.com/recurser/bossanova/commit/2fc8ede002f134fe5318f8a9846190552702fcfa))
* **tui:** [[#83](https://github.com/recurser/bossanova/issues/83)] improve navigation consistency across all views ([c8097dd](https://github.com/recurser/bossanova/commit/c8097ddc49ac46ff282f8a1a019336f1cd1b55d2))
* **tui:** [[#87](https://github.com/recurser/bossanova/issues/87)] remove Plan a feature option and reorder session types ([0680199](https://github.com/recurser/bossanova/commit/0680199e42b53a162ab6f2a0a6189badf4ff1859))
* **tui:** [[#90](https://github.com/recurser/bossanova/issues/90)] add Linear repo settings with API key and team key configuration ([14419da](https://github.com/recurser/bossanova/commit/14419da926a4f625884f6f3b997350ca641be23f))
* update AttachSession RPC to stream from Claude ring buffer ([dbd9699](https://github.com/recurser/bossanova/commit/dbd9699a62a5af4f89e8f09b13f1860fd2ba4504))
* **vcs:** [[#12](https://github.com/recurser/bossanova/issues/12)] add PRDisplayStatus enum, ComputeDisplayStatus, and proto definition ([166c092](https://github.com/recurser/bossanova/commit/166c092a817b10009a7a38ef5dd4f2ebc7c70789))
* **vcs:** add draft PR display status across the stack ([9143eb1](https://github.com/recurser/bossanova/commit/9143eb1988f3a031fb8715aa9748b7aa4d2b7ce3))
* **web:** [[#3](https://github.com/recurser/bossanova/issues/3)] migrate from ESLint to Biome and add path aliases ([16b5b49](https://github.com/recurser/bossanova/commit/16b5b497a9b8717bb8bfa9e7d3d1c2b72aaa10cc))
* **web:** add CF Pages deployment config and fix Vite 8 build ([6753618](https://github.com/recurser/bossanova/commit/6753618a8e713a373784aa4fabb984c8daf7f795))
* **web:** add React Router layout with nav and auth controls ([2e72af2](https://github.com/recurser/bossanova/commit/2e72af2a23cfb23990d574904b20ba791d0d6902))
* **web:** add session action buttons (stop, pause, resume, transfer) ([52ec629](https://github.com/recurser/bossanova/commit/52ec629513f738b9e5f2238c6fcb667c58f26f97))
* **web:** add session detail page with server-streaming output ([a48ab2a](https://github.com/recurser/bossanova/commit/a48ab2aa4157eeb3ba2f56cf8a46605882461ccb))
* **web:** build daemon list page with ListDaemons polling ([5ca4946](https://github.com/recurser/bossanova/commit/5ca4946d697703c30c9986e2956a5b5c91048f01))
* **web:** build session list page with polling ([142a888](https://github.com/recurser/bossanova/commit/142a8887414f1e0840eb4b3f6a374ddef1269e32))
* **web:** create ConnectRPC transport with Auth0 JWT interceptor ([5c19075](https://github.com/recurser/bossanova/commit/5c190751183409f1a4f465b8dfabe4b9f538fe31))
* **web:** install SPA dependencies ([ac5e26e](https://github.com/recurser/bossanova/commit/ac5e26ef26a8d46067498316ea325dd30e813217))
* **web:** scaffold React SPA with Vite + TypeScript ([c45b057](https://github.com/recurser/bossanova/commit/c45b05746869e2c4caba9fc1be50be8c00bff21c))
* **web:** set up Auth0 provider with PKCE flow ([fafd943](https://github.com/recurser/bossanova/commit/fafd943f65c97bba4db82bacbd673e1939ffb85d))
* wire bossd daemon entry point with DB, migrations, and signal handling ([480d5c0](https://github.com/recurser/bossanova/commit/480d5c0ed4ff96041d56974c104e144f00b6c6de))
* wire SessionLifecycle into Server and daemon entry point ([cb903ec](https://github.com/recurser/bossanova/commit/cb903ec9245a1d8e9a225ff329cc84f8cff9f3ab))
* wire VCS provider into Server and daemon, implement ListRepoPRs ([9e791b1](https://github.com/recurser/bossanova/commit/9e791b18a3b3d13a5989716a25bebda887090291))

### Bug Fixes

* **auth:** [[#111](https://github.com/recurser/bossanova/issues/111)] add aud claim to test JWT helpers ([077f419](https://github.com/recurser/bossanova/commit/077f41961674f81d35b66f013c307588d441ecc1))
* **auth:** [[#111](https://github.com/recurser/bossanova/issues/111)] add JWT audience validation for 'bosso' ([ea0e291](https://github.com/recurser/bossanova/commit/ea0e2912ba4458f2081bb7a8e196667f6338aa40))
* **auth:** [[#112](https://github.com/recurser/bossanova/issues/112)] set keychain item label to fix repeating password prompt ([24f507d](https://github.com/recurser/bossanova/commit/24f507dc3c071f995035a9247bce782584597644))
* **autopilot:** [[#26](https://github.com/recurser/bossanova/issues/26)] pause workflow when fewer legs complete than expected ([521f257](https://github.com/recurser/bossanova/commit/521f257514f785e6f3d20533aecfccf2df00b710))
* **autopilot:** [[#37](https://github.com/recurser/bossanova/issues/37)] fix flight leg counting and handoff directory resolution ([92e4c86](https://github.com/recurser/bossanova/commit/92e4c866c7ac74ca94925a018f67b30eff2c0835))
* **autopilot:** [[#43](https://github.com/recurser/bossanova/issues/43)] synthesize handoff file when recovery agent doesn't write one ([b0b4412](https://github.com/recurser/bossanova/commit/b0b441233ae7bcc8fdcb50193029b004eae382b5))
* **autopilot:** [[#75](https://github.com/recurser/bossanova/issues/75)] add cleanup leg and stale-progress detection to resolve stuck runs ([72fd9af](https://github.com/recurser/bossanova/commit/72fd9af734dba66bc532d548abf684821bb91de0))
* **autopilot:** [[#79](https://github.com/recurser/bossanova/issues/79)] use bd list instead of bd ready for task counting ([ba6624c](https://github.com/recurser/bossanova/commit/ba6624c3b1fa56a0ab2f7e52468968171f409fe6))
* **autopilot:** [[#81](https://github.com/recurser/bossanova/issues/81)] resume handoff loop at persisted flight leg ([90e052e](https://github.com/recurser/bossanova/commit/90e052edb423a884db25208d8c5bce7385a8aabb))
* **autopilot:** [[#88](https://github.com/recurser/bossanova/issues/88)] fall back to working directory for flight leg counting ([00faea0](https://github.com/recurser/bossanova/commit/00faea0dcb405e1685e90b07d0b9d894b15b7026))
* **autopilot:** add stack trace to panic recovery ([a9b389d](https://github.com/recurser/bossanova/commit/a9b389d0e0a3a2725eb4ea07dd04e9b18e2a1980))
* **autopilot:** pass prompt as CLI arg instead of stdin pipe in tmux ([7888f04](https://github.com/recurser/bossanova/commit/7888f0415c4c0a9559e7b548d7249d6ffef1e06e))
* **autopilot:** use stdin redirect from plan file instead of CLI arg ([398ff2f](https://github.com/recurser/bossanova/commit/398ff2ff71f13d1a1529dce436626172959296a6))
* **boss,bossd:** wrap error messages in TUI and add daemon error logging ([3102946](https://github.com/recurser/bossanova/commit/31029469de3b8dd0d897b2d22842487baf5502a4))
* **boss:** [[#16](https://github.com/recurser/bossanova/issues/16)] fix squashed/missing history on Claude Code reattach ([7a4b247](https://github.com/recurser/bossanova/commit/7a4b247028a7473d2621ce798e754fd28c700a0c))
* **boss:** [[#19](https://github.com/recurser/bossanova/issues/19)] increase question detection tail size from 2048 to 4096 bytes ([886ac20](https://github.com/recurser/bossanova/commit/886ac20980895e07c925ffc8bcd562da7028854f))
* **boss:** [[#21](https://github.com/recurser/bossanova/issues/21)] fix nil pointer dereference in existing PR wizard ([81122d5](https://github.com/recurser/bossanova/commit/81122d5117cae5a92193fa2c2a3f32c9ec728cbf))
* **boss:** [[#31](https://github.com/recurser/bossanova/issues/31)] write interactive prompt and success message to stderr ([3311b5e](https://github.com/recurser/bossanova/commit/3311b5e835ff9efa7945bf447bb0d8ffdf271dbc))
* **boss:** [[#39](https://github.com/recurser/bossanova/issues/39)] strip XML tags from session names in title extraction ([a29abcb](https://github.com/recurser/bossanova/commit/a29abcb3ddce366374f4b42bfaf354723161c48d))
* **boss:** [[#4](https://github.com/recurser/bossanova/issues/4)] fix STATUS alignment, CI workflows, and add merge conflict check ([853232d](https://github.com/recurser/bossanova/commit/853232d348dd70e12dc4793431b5c9e9cb7b601e))
* **boss:** [[#40](https://github.com/recurser/bossanova/issues/40)] always return to chat picker on detach ([b6fdd28](https://github.com/recurser/bossanova/commit/b6fdd2870b8e073f7fb2bd37c9400f77e8fcc6a5))
* **boss:** [[#42](https://github.com/recurser/bossanova/issues/42)] preserve cursor position when returning from detail views ([6d11a3b](https://github.com/recurser/bossanova/commit/6d11a3b217ed4ae700beddaba5f7ab68fb63faf2))
* **boss:** [[#55](https://github.com/recurser/bossanova/issues/55)] account for banner overhead in all TUI table heights ([d968c8d](https://github.com/recurser/bossanova/commit/d968c8d09d14249ecb8f56398d9192834e2d0d80))
* **boss:** [[#57](https://github.com/recurser/bossanova/issues/57)] show "checking" instead of "rejected" when re-checking after rejection ([8c6f48d](https://github.com/recurser/bossanova/commit/8c6f48d016604cb6b1280af425813c93ae0c7c70))
* **boss:** [[#67](https://github.com/recurser/bossanova/issues/67)] apply strikethrough style to closed PRs in session list ([547585e](https://github.com/recurser/bossanova/commit/547585ea4de1d8dfa62e55da641491f5cf04bf61))
* **boss:** add missing newline before action bar in confirm dialog ([64dfd0c](https://github.com/recurser/bossanova/commit/64dfd0cb9dca80470ede5100a7fd452f2a765727))
* **boss:** clear setup output lines on session creation retry ([064fb33](https://github.com/recurser/bossanova/commit/064fb33df0b802719109d4d5cf788e033acb8498))
* **bossd:** [[#46](https://github.com/recurser/bossanova/issues/46)] check thread resolution before promoting bot reviews ([e32c756](https://github.com/recurser/bossanova/commit/e32c7566fc40aad5b1f70d3c14e9b7e5a8627a10))
* **bossd:** [[#47](https://github.com/recurser/bossanova/issues/47)] normalize GraphQL bot logins for thread author matching ([82b64bc](https://github.com/recurser/bossanova/commit/82b64bc45c765fd76dc27e4ba922a5e54cda2020))
* **bossd:** [[#49](https://github.com/recurser/bossanova/issues/49)] prevent autopilot from landing while tasks remain ([0bb9cc6](https://github.com/recurser/bossanova/commit/0bb9cc60b276f2ff9f684b3efecb2415832e3894))
* **bossd:** [[#50](https://github.com/recurser/bossanova/issues/50)] stop autopilot spinning empty legs after plan work completes ([ff1182f](https://github.com/recurser/bossanova/commit/ff1182fff11f84de66820df71ac3bd68b3977e7f))
* **bossd:** [[#54](https://github.com/recurser/bossanova/issues/54)] gracefully handle corrupted git repos when archiving sessions ([5df859d](https://github.com/recurser/bossanova/commit/5df859d227983c7e020d7fe660f8789f742538f4))
* **bossd:** [[#59](https://github.com/recurser/bossanova/issues/59)] change unresolvedThreadAuthors from fail-open to fail-closed ([26c8b2e](https://github.com/recurser/bossanova/commit/26c8b2eb540c2f97f0fe3ed36145713afd86a279))
* **bossd:** [[#86](https://github.com/recurser/bossanova/issues/86)] prevent PR creation failure for repos with empty origin_url ([16ceb6a](https://github.com/recurser/bossanova/commit/16ceb6aac9e6861667efff04b95b32add24e0bd5))
* **bossd:** convert git URLs to owner/repo format for gh CLI ([da0c56f](https://github.com/recurser/bossanova/commit/da0c56f67b1c98dfac0802f10803a6ac143b67d1))
* **bossd:** create empty commit before draft PR to avoid GitHub rejection ([9f9cb95](https://github.com/recurser/bossanova/commit/9f9cb95f6ffdab9f49d68261ec5fa6eb69248401))
* **boss:** dereference pointer returns in handleFormCompleted to avoid panic ([16e90e5](https://github.com/recurser/bossanova/commit/16e90e5a114e07ca7b23aba55b34f22f2b8a006f))
* **bossd:** persist auto-discovered plugins to settings on first run ([dfdd3f2](https://github.com/recurser/bossanova/commit/dfdd3f261f55f9e2db7634f09b0df34c44d41c8c))
* **bossd:** populate IsRepairing field in gRPC ListSessions ([a2ad730](https://github.com/recurser/bossanova/commit/a2ad730cb3a58b1c5232e1774465107db979fb75))
* **bossd:** prevent autopilot stuck in "running" after goroutine exits ([cae8e86](https://github.com/recurser/bossanova/commit/cae8e863420c993cb7cf21c2aacd4663d3895aba))
* **bossd:** prevent deadlock when scanner fails during setup streaming ([60e8e74](https://github.com/recurser/bossanova/commit/60e8e74ce38113303800cca4b42a135ec5324d43))
* **bossd:** tighten flight leg regex to exclude sub-headings ([18c79ea](https://github.com/recurser/bossanova/commit/18c79ea6edfbba5b34569cb53e5dc41d68cc3c16))
* **bossd:** use 'chore: [skip ci] create pull request' as empty commit message ([efd4f6d](https://github.com/recurser/bossanova/commit/efd4f6d3c42a3e1a1ba824f1fe40e0cda1eb0f4e))
* **bossd:** use correct gh pr checks JSON fields (state, workflow) ([a5b3f77](https://github.com/recurser/bossanova/commit/a5b3f7714bc2c8bceda2d649365ad53eb1d3cfd8))
* **bossd:** wrap multi-statement Delete in transaction ([2c76edf](https://github.com/recurser/bossanova/commit/2c76edfe2c37b6948cff33a7dee97cb4f84a1077))
* **bossd:** write setup script output to daemon logs when not streaming ([afa37bf](https://github.com/recurser/bossanova/commit/afa37bfac66a931bb15ab9d7a9309283a6d40bce))
* **boss:** fix spacing in branch overwrite confirmation dialog ([f37a356](https://github.com/recurser/bossanova/commit/f37a356bbda1735988e40f2082a7035e79aeb9e2))
* **boss:** heap-allocate huh form-bound values to fix stale pointer bug ([3739c0e](https://github.com/recurser/bossanova/commit/3739c0e97cb090a6353b50fd3f17c67dc90f245c))
* **boss:** prevent single-session handlers from resetting bulk operation flags ([75b0096](https://github.com/recurser/bossanova/commit/75b0096b48030c9d1c996fb014b4185cf05859fb))
* **boss:** remove duplicate banner from home view ([a12384d](https://github.com/recurser/bossanova/commit/a12384d090d2e7e73b7d0932b2c56eacf3711d34))
* **boss:** reset confirmingAll/deletingAll flags in single-session handlers ([559ef48](https://github.com/recurser/bossanova/commit/559ef487949841701393df9a1db686c920b71b43))
* **boss:** show session info in banner for ViewSessionSettings ([dc91b19](https://github.com/recurser/bossanova/commit/dc91b1979a544a6dfa0cc0be73ab3de42e42af0e))
* **boss:** use consistent confirmation dialog pattern for branch overwrite ([743a55b](https://github.com/recurser/bossanova/commit/743a55ba4babec6c30704065bef114e26ab7843f))
* **build:** [[#111](https://github.com/recurser/bossanova/issues/111)] add .gitkeep to skills/ so go:embed pattern resolves in fresh clones ([b7e6077](https://github.com/recurser/bossanova/commit/b7e607722aa76d9ed60bbd611565d09573cdf961))
* **build:** [[#6](https://github.com/recurser/bossanova/issues/6)] add web node_modules prerequisite to generate target ([7779fa6](https://github.com/recurser/bossanova/commit/7779fa6b0f75cf8ce5bff50f777536f239b61ab2))
* **build:** handle empty skills directory in public repo ([64e8456](https://github.com/recurser/bossanova/commit/64e8456c72ceb106d340e7b7686082ffa33a5069))
* **ci:** [[#111](https://github.com/recurser/bossanova/issues/111)] use GITHUB_PATH instead of env PATH override for protoc-gen-es ([5b8d5cc](https://github.com/recurser/bossanova/commit/5b8d5cc745796821d16de32a76470900ab048107))
* **ci:** add golangci-lint install step to all Go workflows ([d35ecd8](https://github.com/recurser/bossanova/commit/d35ecd8f3b4a0a937ba3cba7dd197e6735d8e3e2))
* **ci:** add persist-credentials: false to release and homebrew checkouts ([9a56201](https://github.com/recurser/bossanova/commit/9a56201cd7196e1aa618bfe442a321f4dca2ab6b))
* **ci:** commit generated protobuf code for CI builds ([3b0c5ad](https://github.com/recurser/bossanova/commit/3b0c5ad33a0c8179545c5b3a1054c4dad1da878c))
* **ci:** correct semantic-release plugin version specifiers ([6512632](https://github.com/recurser/bossanova/commit/6512632177d5ba6b5e2acb4ee568fc8c9174fa65))
* **ci:** create Formula/ directory for fresh homebrew-tap repo ([9b45753](https://github.com/recurser/bossanova/commit/9b457537834713b984b41727f82c532b19a1155d))
* **ci:** install golangci-lint via curl, fix v2 config exclude-dirs ([38de367](https://github.com/recurser/bossanova/commit/38de3671c2d756d1b67f4282d9cb63559b23d545))
* **ci:** mirror only production to public repo's main branch ([2f0fa69](https://github.com/recurser/bossanova/commit/2f0fa69e8177fdbc9df82c4f6966c457dbb556ce))
* **ci:** strip .beads/ from public mirror, remove deprecated split workflow ([ee6a3b3](https://github.com/recurser/bossanova/commit/ee6a3b3847ada773b554ab3a281d2ca0ff2095ee))
* **ci:** strip .claude and .husky from public mirror ([9d6827f](https://github.com/recurser/bossanova/commit/9d6827f69c9cdfdfff9391b642c75508d4b94f8b))
* **ci:** strip v prefix from semantic-release version output ([939bebb](https://github.com/recurser/bossanova/commit/939bebbd16d77e0e957f80982501348b3a203056))
* **ci:** upgrade artifact actions to Node 24, fix Go cache, skip notarize gracefully ([8e52763](https://github.com/recurser/bossanova/commit/8e52763eea99656531ff171c3ca627c58457c170))
* **ci:** use default token for checkout in release and homebrew jobs ([d8906a2](https://github.com/recurser/bossanova/commit/d8906a2cd3e1b4dfee7a6a2d77ce7ad0ec8b598e))
* **ci:** use per-module targets in main CI, strip infra/ from public mirror ([f79fdbe](https://github.com/recurser/bossanova/commit/f79fdbe5fd8b6343c32847f07875977fda7b4954))
* **daemon:** add --tsconfig flag to tsx dev script for path alias resolution ([4827ca6](https://github.com/recurser/bossanova/commit/4827ca68ec8ce80d7e4d7b0ee8ef9022e27c7254))
* **daemon:** use singleton DI registration and top-level ESM imports ([bbec4ff](https://github.com/recurser/bossanova/commit/bbec4ffbb4c996e1cffa4a45b3eb3509d504a8b6))
* **dependabot:** [[#33](https://github.com/recurser/bossanova/issues/33)] switch from lazy to eager broker dialing ([d1d9009](https://github.com/recurser/bossanova/commit/d1d9009db49db6645a62e48ed6e7de5294919c20))
* **deploy:** [[#111](https://github.com/recurser/bossanova/issues/111)] wire Terraform + deploy config for bosso and web ([35f1dc0](https://github.com/recurser/bossanova/commit/35f1dc06603714cadc60e3ccb79ca8ecfb236ef5))
* **deploy:** [[#111](https://github.com/recurser/bossanova/issues/111)] wire Terraform Cloud + branch-triggered deploy pipeline ([e02c4ba](https://github.com/recurser/bossanova/commit/e02c4ba66da56c7ee6a88819c6989caac57f7e2e))
* **deps:** regenerate package-lock.json from scratch ([3ce1696](https://github.com/recurser/bossanova/commit/3ce16964a3d26102606de32ad3ebd5ccc9298f42))
* **deps:** update package-lock.json for vite 8.0.5 ([69e93ac](https://github.com/recurser/bossanova/commit/69e93acaa4804b9a549650e92affb76071046575))
* gap analysis fixes — ListSessions, EmptyTrash, orphaned sessions, lint ([707df8f](https://github.com/recurser/bossanova/commit/707df8f9c1c2630aeff2074555a01ad9425f8ad3))
* **global:** fix fetch-depth during mirroring ([a31a1ab](https://github.com/recurser/bossanova/commit/a31a1ab47e6196cf1864fb78bd29ba2e4ce8dbc8))
* **global:** make sure the release build gets triggered ([bdfbd0b](https://github.com/recurser/bossanova/commit/bdfbd0bba71e80a63f6e74d6e330354941c60c70))
* **global:** rename the mirror-public action's GITHUB_TOKEN env-var to avoid collisions ([3661e52](https://github.com/recurser/bossanova/commit/3661e5222c32e94cd1367d72ad58164f52556838))
* **global:** trigger a release ([610c656](https://github.com/recurser/bossanova/commit/610c656cb74a1fee533c2ffe93caea477f85bd7f))
* **global:** trigger a release ([128869b](https://github.com/recurser/bossanova/commit/128869be5dfc1f07e570d82a8ac2d353883b423e))
* **global:** trigger a release ([e8b3e76](https://github.com/recurser/bossanova/commit/e8b3e763293ec1715da6ea2b6025d9ab85c3c6d3))
* **global:** trigger a release ([1b68301](https://github.com/recurser/bossanova/commit/1b683017979847eaae50a308d6191bd5c22cc925))
* **global:** trigger a release ([0097438](https://github.com/recurser/bossanova/commit/0097438eeaaac328e70e2efaab9646e5fb64fc53))
* **global:** trigger a release ([0f80f61](https://github.com/recurser/bossanova/commit/0f80f61eaa9bcaa406f905940f2ab1be3d6207cf))
* **homebrew:** remove post_install hook that fails in sandbox ([05d99dd](https://github.com/recurser/bossanova/commit/05d99ddd4bcbb8ded84af848abd98b3008cfb593))
* **homebrew:** set executable permission on plugin binaries ([5f30a2a](https://github.com/recurser/bossanova/commit/5f30a2a25d3e9ea5e7e86a104446764bf95bbcf0))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] remove Fly module from Terraform, manage with flyctl only ([0898031](https://github.com/recurser/bossanova/commit/08980318143431c756810c945d745d003120b706))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] set Fly primary region to ams (Amsterdam) ([9404f10](https://github.com/recurser/bossanova/commit/9404f1086edc0d7b7ad0eb0af8b1ad708db3857f))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] set R2 bucket location to WEUR (Western Europe) ([ab63efd](https://github.com/recurser/bossanova/commit/ab63efd40cddb2d61a1f643bcc3e7dcb4ad1760a))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use 'personal' as Fly.io org slug ([9b6638b](https://github.com/recurser/bossanova/commit/9b6638b3e5304a71dc565e8dcf2c9b4eee601c50))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use correct fly provider version constraint ~> 0.0.20 ([858e7d1](https://github.com/recurser/bossanova/commit/858e7d155038374393084026c482601921be254a))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use correct Fly.io org slug dave-perrett ([a17e626](https://github.com/recurser/bossanova/commit/a17e6264d47d3037b2405bd911e24fffce527928))
* **infra:** [[#111](https://github.com/recurser/bossanova/issues/111)] use separate WorkOS client IDs for staging and production ([7fc2693](https://github.com/recurser/bossanova/commit/7fc26938092fc89f7d3ac34b386f0a1d3a413359))
* **migration:** use table-recreation for SQLite < 3.35 compatibility ([f2720f4](https://github.com/recurser/bossanova/commit/f2720f4298d50c764e328e13c8133a0dc69fe482))
* **mirror:** preserve public repo history across releases ([c53070e](https://github.com/recurser/bossanova/commit/c53070e6d610952895981debccbe6d045458e081))
* **mirror:** remove hardcoded plugin name references from public-visible code ([bbc0f64](https://github.com/recurser/bossanova/commit/bbc0f6479b82008096e445d78107e39490e3ec3e))
* **mirror:** remove private module references from public repo build ([84488fd](https://github.com/recurser/bossanova/commit/84488fd5fc8490e6568bfa6ad7277b848aac33a1))
* **mirror:** use orphan commit to prevent private history leaking ([4397612](https://github.com/recurser/bossanova/commit/439761219e2e2ec8be65d4918367e11d1879b551))
* **mirror:** use read-tree to populate index during commit replay ([64185c1](https://github.com/recurser/bossanova/commit/64185c18cdc84afda32331c80f0f6a0276095cf0))
* **mutate:** [[#72](https://github.com/recurser/bossanova/issues/72)] add --dangerously-skip-permissions to claude pipe mode ([5159120](https://github.com/recurser/bossanova/commit/515912020c96c54f98fdc5dcb22d7de55eb70b7d))
* **mutate:** address review feedback on Makefile targets ([db76add](https://github.com/recurser/bossanova/commit/db76add0116b90ddb2228838bc50a3d151330970))
* **orchestrator:** [[#108](https://github.com/recurser/bossanova/issues/108)] stop retrying failed task mappings to prevent duplicate sessions ([63df67a](https://github.com/recurser/bossanova/commit/63df67ab70c03f1ddc5f5169e38c8a76d4f138de))
* **orchestrator:** [[#28](https://github.com/recurser/bossanova/issues/28)] resolve four edge cases preventing dependabot PR auto-merge ([8671cf2](https://github.com/recurser/bossanova/commit/8671cf27f1dbfc0d3097987593302d586b014b47))
* **orchestrator:** [[#30](https://github.com/recurser/bossanova/issues/30)] use vcs.GitHubNWO to construct PR URLs from git remote origins ([c9d07d6](https://github.com/recurser/bossanova/commit/c9d07d656bc2626e806ed9b412c1bdac300f3afb))
* **orchestrator:** [[#35](https://github.com/recurser/bossanova/issues/35)] check soft failures on smartRetry attempt ([b854c51](https://github.com/recurser/bossanova/commit/b854c5183bea5dda03ebf2ddc83dcc7c2593c7ad))
* **orchestrator:** [[#73](https://github.com/recurser/bossanova/issues/73)] wire up HandleSessionCompleted to unblock dependabot task queue ([d9bde97](https://github.com/recurser/bossanova/commit/d9bde9758520973c871f34eb9fba4cc5e084418e))
* **orchestrator:** [[#94](https://github.com/recurser/bossanova/issues/94)] add stale queue recovery for stuck task mappings ([d33207f](https://github.com/recurser/bossanova/commit/d33207f9bbad01d7c6f6edfd40bec395f273afa7))
* **orchestrator:** pass PR number and URL when creating sessions from tasks ([723ee5f](https://github.com/recurser/bossanova/commit/723ee5ffcf48747886becfae7b76d99bfb8cc281))
* **pilot:** [[#82](https://github.com/recurser/bossanova/issues/82)] show correct flight leg number for completed workflows ([3d0fd54](https://github.com/recurser/bossanova/commit/3d0fd540741d77e108c834c158589613854de6f6))
* **plugin:** [[#18](https://github.com/recurser/bossanova/issues/18)] check only most recent PR for rejection detection ([1c15044](https://github.com/recurser/bossanova/commit/1c150442d503e5380cdfb81fa7cdcd94bedebb0d))
* **plugin:** [[#22](https://github.com/recurser/bossanova/issues/22)] probe GetInfo before caching dispensed interfaces ([8447fb6](https://github.com/recurser/bossanova/commit/8447fb61ff494f781d66bc803007727b900afa3c))
* **plugin:** restore HostService broker registration on TaskSourceGRPCPlugin ([9ec6a9e](https://github.com/recurser/bossanova/commit/9ec6a9ece93cd3ee4b8215c2593a766526b0b02d))
* **polling:** [[#36](https://github.com/recurser/bossanova/issues/36)] reduce GitHub API polling to avoid rate limits ([8beee30](https://github.com/recurser/bossanova/commit/8beee30d2b98e8d255085477687e3ea45f561e26))
* **pty:** [[#23](https://github.com/recurser/bossanova/issues/23)] remove 500-char limit on question detection after ⏺ marker ([2364b97](https://github.com/recurser/bossanova/commit/2364b97723577f9ca65c16b3caa18687f0e31570))
* **pty:** [[#29](https://github.com/recurser/bossanova/issues/29)] detect questions when ⏺ marker is outside tail buffer ([44d5bba](https://github.com/recurser/bossanova/commit/44d5bbaf0b0dd9ac88151f8893cf3ab54fa33984))
* remove unused proto conversion helpers to pass lint ([da99008](https://github.com/recurser/bossanova/commit/da9900858079837a75b6b8a32676ca12ae6517bf))
* **repair:** [[#109](https://github.com/recurser/bossanova/issues/109)] skip repair when head commit SHA matches last attempt ([063f7a6](https://github.com/recurser/bossanova/commit/063f7a624c785232edab5b82a3ad3b5ccc164658))
* **repair:** [[#66](https://github.com/recurser/bossanova/issues/66)] add periodic sweep to catch stuck sessions ([7573112](https://github.com/recurser/bossanova/commit/7573112ccee0de8570e4e4537bc046298f611a7c))
* **repair:** reduce default cooldown between repair attempts from 5 to 2 minutes ([27ec4d5](https://github.com/recurser/bossanova/commit/27ec4d568a16e26f24faefb576de01a3e200e5f9))
* resolve CI lint failures across boss, bossd, and web ([51e4dd3](https://github.com/recurser/bossanova/commit/51e4dd3159b4567fb39338a79b06f2288b3d2d6b))
* resolve errcheck lint issues and update golangci-lint config for v2 ([01ad6ee](https://github.com/recurser/bossanova/commit/01ad6ee4d0573668a9ce65712aa2a7b31f8cdd5d))
* resolve exhaustive switch and Slowloris lint warnings ([17cd38f](https://github.com/recurser/bossanova/commit/17cd38f1642c534159e5a7795e4b8afea127ce0b))
* resolve lint issues in boss CLI (errcheck, unused field) ([77e66ff](https://github.com/recurser/bossanova/commit/77e66ff3fb4a00d79c0397d37f516e9b962216b2))
* **review:** [[#111](https://github.com/recurser/bossanova/issues/111)] add middleware exemption tests and CF Pages preview config ([dfe9616](https://github.com/recurser/bossanova/commit/dfe961667bf688e019619c14a822153bdc9934f8))
* **session:** [[#74](https://github.com/recurser/bossanova/issues/74)] make draft PR creation non-fatal and clean up resources on failure ([a0834ce](https://github.com/recurser/bossanova/commit/a0834ce06df5c4a142b312638c6896286fb588f9))
* **session:** reconcile orphaned sessions with existing PRs on startup ([acc821c](https://github.com/recurser/bossanova/commit/acc821c8b1b2c50e465577acc69fdcaef93ca862))
* **shared:** fix max attempts guard and add session machine tests ([7f5440d](https://github.com/recurser/bossanova/commit/7f5440d3f533aaf653d753add90b3a4db31f053b))
* **status:** [[#108](https://github.com/recurser/bossanova/issues/108)] capture tmux scrollback to prevent false positive question detection ([55b2eae](https://github.com/recurser/bossanova/commit/55b2eae1cf2e18249b4a90826fd67ef8d2d0fb6f))
* **status:** [[#113](https://github.com/recurser/bossanova/issues/113)] sessions show idle instead of working on daemon startup ([eb6e511](https://github.com/recurser/bossanova/commit/eb6e5118d2b0730630dabe5acf1514b3877e8cc1))
* **status:** [[#77](https://github.com/recurser/bossanova/issues/77)] correct display for merged PRs and unknown mergeability ([9b50dc4](https://github.com/recurser/bossanova/commit/9b50dc4aca8447497841af1b7bc467c14dbd2abb))
* **status:** [[#78](https://github.com/recurser/bossanova/issues/78)] exclude failed/cancelled workflows from active query ([b3f4d7f](https://github.com/recurser/bossanova/commit/b3f4d7f9d34cd0e73ecef11a8706aea64a7492e6))
* **status:** deduplicate constructPRURL and fix mergeable-unknown idle regression ([7b02568](https://github.com/recurser/bossanova/commit/7b025682d64888436606d923d10f3cbca4d27635))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] add csi-u key format and TERM_PROGRAM passthrough ([4b5f8f7](https://github.com/recurser/bossanova/commit/4b5f8f7594d6968d28b6277763e77c955d999e39))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] fix key bindings, extended-keys, and enable mouse scrolling ([b101f45](https://github.com/recurser/bossanova/commit/b101f45492817cee2b8338f75b989ec500088226))
* **tmux:** [[#95](https://github.com/recurser/bossanova/issues/95)] set mouse mode globally and add Shift+Enter test scaffolding ([87faede](https://github.com/recurser/bossanova/commit/87faede7e3d1d04f01a86bb13a6cd98d738462f7))
* **tui:** [[#106](https://github.com/recurser/bossanova/issues/106)] remove misleading "ready for review" attention alert ([82290fe](https://github.com/recurser/bossanova/commit/82290fe7a9af5d20100db1e657dbe06b93f883ea))
* **tui:** [[#91](https://github.com/recurser/bossanova/issues/91)] make Esc navigate back one step instead of jumping to home ([8116ca1](https://github.com/recurser/bossanova/commit/8116ca1146011dcbfad580a13d263aedc7169b86))
* **tui:** [[#92](https://github.com/recurser/bossanova/issues/92)] preserve list cursor position after navigating back ([98fb485](https://github.com/recurser/bossanova/commit/98fb485b0749023c9e55f1f726cad3932b77f2b8))
* **tui:** correct table height overhead in sub-views ([90a65b1](https://github.com/recurser/bossanova/commit/90a65b1db4aea5e8f34fa4c9353997b89a5962c6))
* **tui:** remove extra blank line below banner across all screens ([d0c0f00](https://github.com/recurser/bossanova/commit/d0c0f009d670528dc1ec2b994b6b9ea8178e3e0e))
* **tui:** remove extra blank line below header in create session screen ([cbf4d65](https://github.com/recurser/bossanova/commit/cbf4d6515df63ceeb2d76ccba99058c9bf99cd4f))
* **vcs:** [[#32](https://github.com/recurser/bossanova/issues/32)] return informational error when repo has no real commits ([46bde5b](https://github.com/recurser/bossanova/commit/46bde5b25424dbedad72b0dd9287e7f6f819550a))
* **vcs:** [[#34](https://github.com/recurser/bossanova/issues/34)] retry CreateDraftPR on GitHub API not-ready errors ([8f8a92d](https://github.com/recurser/bossanova/commit/8f8a92dce44a3c4d8873612a6075a2485079b77b))
* **web:** [[#111](https://github.com/recurser/bossanova/issues/111)] migrate services/web from npm to pnpm workspace ([797ad1c](https://github.com/recurser/bossanova/commit/797ad1c0af57e431f9b42f2d32d4a0e2d95f6f85))

### Performance Improvements

* **repair:** [[#85](https://github.com/recurser/bossanova/issues/85)] reduce repair turnaround timing constants ([d3c3e1d](https://github.com/recurser/bossanova/commit/d3c3e1d40d450cc0bb3f217b9fcae11a91c1b6f7))
