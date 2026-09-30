# Changes needed in Sprinkles to support GPU job submission

Grounded in the current Sprinkles server/worker code in
`sparklespray/cli` (Go), not the old Python Sparkles implementation in
`sparkles-stable`. Sprinkles here is: an HTTP API (`dev/dashboard_backend.go`,
`POST /api/v1/job`) → Firestore-backed workpool/monitor state
(`monitor/*.go`) → GCP Batch API job creation (`monitor/batch_api.go`) →
a worker binary that `docker run`s each task (`worker.go`). GPU support
needs one new field threaded through every layer of that pipeline, plus a
GPU driver/boot-disk/docker-flag change at the two layers that talk to GCP
and to docker.

There is no separate "Sparkles" backend in this codebase to mirror —
`sprinkles-gpu.md`'s references to `batch_api.py` describe the old Python
CLI in `sparkles-stable`, which is a different, unrelated implementation.
The mapping table it documents (accelerators → `batch.AllocationPolicy.Accelerator`,
`provisionMode` → `ProvisioningModel`, GPU machine types → `install_gpu_drivers`

- `batch-debian`) is still the right target behavior; it just needs a new,
  Go-native implementation here.

---

## 1. New `workpool` JSON fields

`dev/workpool_spec.go:14` (`WorkpoolSpec`) needs two new fields, matching
the shape `sprinkles-gpu.md` specifies:

```go
type WorkpoolSpec struct {
    ...
    Accelerators  []sprinkles.Accelerator `json:"accelerators"`
    ProvisionMode string                  `json:"provisionMode"`
}
```

where `Accelerator{Type string; Count int}` is a new type (mirroring
`ResourceEntry`/`EmptyVolume` in `task_queue.go:55-68`), since no
accelerator-shaped type exists yet anywhere in this codebase.

`applyWorkpoolDefaults` (`dev/dashboard_backend.go:812-837`) needs a default:

```go
if spec.ProvisionMode == "" {
    spec.ProvisionMode = "spot"
}
```

`handleSubmitJob` (`dev/dashboard_backend.go:763-`) should validate
`ProvisionMode` is one of `"spot"`, `"normal"`, `"flex"` alongside
the existing checks at lines 888-909, and pass both new fields into the
`sprinkles.WorkPool{...}` literal at line 937.

## 2. Plumbing the new fields to Firestore and back

Unlike the old Python client, this field needs to survive **three**
separate struct definitions plus one conversion function, because the
Sprinkles server round-trips workpool config through Firestore:

| Type                        | File                       | Role                                                 |
| --------------------------- | -------------------------- | ---------------------------------------------------- |
| `sprinkles.WorkPool`        | `task_queue.go:80`         | written to Firestore by `handleSubmitJob`            |
| `monitor.firestoreWorkPool` | `monitor/adapters.go:37`   | read back from Firestore                             |
| `monitor.WorkPool`          | `monitor/interfaces.go:75` | in-memory type the monitor/provisioner actually uses |

Add `Accelerators []Accelerator` and `ProvisionMode string` (with a
`firestore:"..."` tag) to all three, and add the two fields to the
`toWorkPool()` conversion at `monitor/adapters.go:72-96`, which currently
copies every other field one at a time and would silently drop these two
if left unmodified.

## 3. Provisioning: `monitor/provision.go`

`runProvisioningPollForWorkpool` (`monitor/provision.go:55-122`) currently
always tries to split requested VMs between preemptible (SPOT) and
non-preemptible (STANDARD) based on a rolling zombie-incident budget
(lines 96-113), then calls `submitBatch` once per split
(lines 115-121), passing a `preemptible bool` each time. That split is
specifically what the new `"spot"` provision mode should still do — so:

- `pool.ProvisionMode == "normal"`: skip the zombie-budget split
  entirely and call `submitBatch` once, requesting all `toRequest` VMs
  with mode `"normal"`.
- `pool.ProvisionMode == "flex"`: also skip the split — flex isn't a
  spot/standard tradeoff, it's DWS Flex Start queuing — and call
  `submitBatch` once requesting `toRequest` VMs with mode `"flex"`.
- `pool.ProvisionMode == "spot"` (default): keep the existing
  budget-based split, but pass the resulting mode as a string instead of
  a bool — `submitBatch(..., "spot", ...)` for the preemptible-budget
  share and `submitBatch(..., "normal", ...)` for the rest.

`submitBatch` (`monitor/provision.go:129-`) currently takes a
`preemptible bool` parameter and passes it straight through to
`WorkerJobSpec.Preemptible`. Per the instruction to stop representing this
as a bool: **remove `Preemptible bool` from `WorkerJobSpec`
(`monitor/interfaces.go:211-233`) entirely** and replace it with
`ProvisionMode string`; change `submitBatch`'s parameter from
`preemptible bool` to `provisionMode string` and set
`ProvisionMode: provisionMode` in the `WorkerJobSpec{...}` literal instead
of `Preemptible: preemptible`. `CreateJob` (next section) then switches on
this one string for all three modes instead of branching on a bool plus a
separate mode. Also add `Accelerators []Accelerator` to `WorkerJobSpec`
and pass `pool.Accelerators` into the `WorkerJobSpec{...}` literal at
`monitor/provision.go:130-148`, alongside the existing
`MachineType: pool.MachineType,` line.

## 4. `monitor/batch_api.go` — `CreateJob`

This is the Go equivalent of the Python `create_batch_job_from_job_spec()`
`sprinkles-gpu.md` references, and needs the same four changes, against
the real `google.golang.org/api/batch/v1` types (confirmed via `go doc`):

### 4a. Detect GPU usage

Add a Go equivalent of `_machine_type_has_gpu()`:

```go
var gpuMachinePrefixes = []string{"a2-", "a3-", "a4-", "g2-", "g4-"}

func machineTypeHasGPU(machineType string) bool {
    for _, p := range gpuMachinePrefixes {
        if strings.HasPrefix(machineType, p) {
            return true
        }
    }
    return false
}
```

`CreateJob` (`monitor/batch_api.go:213-`) should compute
`useGPU := len(spec.Accelerators) > 0 || machineTypeHasGPU(spec.MachineType)`
near the top, alongside the existing `provisioningModel` computation at
lines 257-260.

### 4b. Provisioning model + reservation

Lines 257-260 today:

```go
provisioningModel := "STANDARD"
if spec.Preemptible {
    provisioningModel = "SPOT"
}
```

needs to become a three-way switch on the new `ProvisionMode string` field
(replacing `Preemptible bool` — see section 3), also producing a
`reservation` string (currently `InstancePolicy` at lines 334-339 sets no
`Reservation` field at all):

```go
var provisioningModel, reservation string
switch spec.ProvisionMode {
case "flex":
    provisioningModel = "FLEX_START"
    reservation = "NO_RESERVATION"
case "normal":
    provisioningModel = "STANDARD"
default: // "spot"
    provisioningModel = "SPOT"
}
```

### 4c. Accelerators + boot disk image + driver install

`InstancePolicy` (`monitor/batch_api.go:334-339`) and
`InstancePolicyOrTemplate` (`monitor/batch_api.go:332-341`) need three
additions — `batch.Accelerator`, `batch.Disk.Image`, and
`batch.InstancePolicyOrTemplate.InstallGpuDrivers` all already exist in the
vendored `google.golang.org/api/batch/v1` package:

```go
var batchAccelerators []*batch.Accelerator
for _, a := range spec.Accelerators {
    batchAccelerators = append(batchAccelerators, &batch.Accelerator{
        Type: a.Type, Count: int64(a.Count),
    })
}

Instances: []*batch.InstancePolicyOrTemplate{
    {
        InstallGpuDrivers: useGPU,
        Policy: &batch.InstancePolicy{
            BootDisk:          &batch.Disk{SizeGb: ..., Type: ..., Image: "batch-debian"},
            MachineType:       spec.MachineType,
            ProvisioningModel: provisioningModel,
            Reservation:       reservation,
            Disks:             disks,
            Accelerators:      batchAccelerators,
        },
    },
},
```

(Confirmed via `go doc google.golang.org/api/batch/v1.Accelerator`: the
field is `Type`, same name as the Python client's `type_` maps to.)

Note this departs from `sprinkles-gpu.md` and the old Python
implementation, which only use `batch-debian` when `useGPU` is true
(falling back to `batch-cos` otherwise): per direction, boot disk image is
always `"batch-debian"` here, regardless of GPU usage.

## 5. Worker: `--gpus all` on the task's docker run

This is the open question `sprinkles-gpu.md` flags, and the answer is
concrete in this codebase: the worker binary already takes an
`extraDockerArgs []string` for the task's `docker run` (`worker.go:158-179`,
inserted into `args` right before the image name), built by
`buildDockerArgs(bindMounts []string, _ *Task) []string`
(`worker.go:822-828`). There is currently **no** flag or field anywhere
that tells the worker it's running on a GPU machine — that needs to be
added end-to-end:

1. **`cmd/sprinkles worker` flag** — add `cli.BoolFlag{Name: "gpu"}`
   next to the existing `--no-docker`/`--bind-mount` flags
   (`cli_main.go:30-45`).
2. **`monitor/batch_api.go` `CreateJob`** — the `workerArgs` string built
   at `monitor/batch_api.go:294-296` (which already embeds `--stream`,
   `--batch`, `--project`, etc. into the shell command that launches the
   worker binary on the VM) needs to append `--gpu` when `useGPU` is true.
3. **`startWorker`** (`worker.go:856-`) — add a `useGPU bool` parameter,
   threaded through from the two call sites at `worker.go:123` and
   `worker.go:672` (which read the `--gpu` flag / `WorkerLoopConfig`),
   and store it on `workerState` (`worker.go:830-847`).
4. **`buildDockerArgs`** (`worker.go:822-828`) — accept a `useGPU bool`
   parameter and append `"--gpus", "all"` when set, matching how it
   already appends `"-v", bindMount` for each bind mount:

   ```go
   func buildDockerArgs(bindMounts []string, useGPU bool, _ *Task) []string {
       var args []string
       for _, bindMount := range bindMounts {
           args = append(args, "-v", bindMount)
       }
       if useGPU {
           args = append(args, "--gpus", "all")
       }
       return args
   }
   ```

   The call site is `worker.go:415` (`buildDockerArgs(cfg.BindMounts, t)`),
   which needs `cfg.UseGPU` (or equivalent) threaded onto
   `WorkerLoopConfig` alongside `BindMounts`.

Since a workpool has one machine type for its entire lifetime, GPU-ness is
a property of the worker process as a whole, not of an individual task —
which is why this is a worker-level flag rather than something carried per
`*Task` (the unused `_ *Task` parameter in `buildDockerArgs` is dead
weight either way and can be dropped once this change lands, unless a
later change needs per-task docker args for some other reason).

## 6. Things this doc deliberately leaves out

- The `sprinkles submit` CLI subcommand (`submit_cmd.go`) posts a
  user-supplied JSON file to `POST /api/v1/job` more or less verbatim and
  has no accelerator/provision-mode-specific code today — a user can
  already put `"accelerators"` / `"provisionMode"` in their job JSON once
  the server (sections 1-5) accepts them; no client-side change is needed
  there.
- N1-vs-A/G-series accelerator-type validation (the `_N1_GPU_TYPES` table
  in `sprinkles-gpu.md`) doesn't exist anywhere in this Go codebase yet —
  today nothing validates `machineType` against `accelerators` at all.
  Whether that validation belongs in `handleSubmitJob` (section 1) is a
  product decision, not called out further here.
