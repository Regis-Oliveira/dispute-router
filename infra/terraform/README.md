# Terraform, explained through this stack

**Never applied** — there is no AWS account behind this project — but it does
`fmt`, `validate` and **`plan` cleanly**: 60 resources, no errors, against
OpenTofu 1.12.6 and AWS provider 5.100.

A plan is a much stronger check than `validate`. `validate` only sees syntax and
references; `plan` resolves every data source and puts every argument through
the provider's own validation. It was run against LocalStack, which is enough to
compute the graph even though ECS and ELB cannot actually be created there.

What that still does not prove: nothing has been applied, so IAM policies have
never been evaluated by IAM, and no container has ever started.

## How it runs, before anything else

Terraform is **not a service**. There is no daemon, no port, nothing to deploy,
and nothing running between the times you use it. It is a single binary you
invoke like `make` or `npm`: it starts, does work, and exits.

```
$ pgrep -f tofu | wc -l     # before
0
$ tofu plan                 # ... 40 API calls to AWS, then done
$ pgrep -f tofu | wc -l     # after
0
```

What it does while it runs is call the AWS API — `sts.GetCallerIdentity`,
`ec2.DescribeVpcs`, `ecs.RegisterTaskDefinition`. The same API the console calls
when a person clicks around, just called by a program that read a file first.

| | Runs continuously | You invoke it |
| --- | --- | --- |
| nginx, Postgres, the services in this repo | yes | |
| make, npm, git, **terraform** | | yes |

Three commands, and only one of them writes anything:

```
tofu init     once per checkout. Downloads the provider plugin.
tofu plan     read AWS, compare against the code, print the difference. Changes nothing.
tofu apply    make the API calls that close that difference. Then exit.
```

`plan` is read-only, which is what makes it safe to run on every pull request.
`apply` shows the same diff and waits for a yes.

The infrastructure it creates keeps running afterwards. Terraform does not, and
has no idea what happens in between — which is exactly why the state file exists
and why the next section is about it.

**Where it runs in practice:** on a laptop while learning, and in CI for
anything real. A pull request runs `plan` and posts the diff for review; merging
runs `apply`. Nobody applies from a laptop on a team, because then the state and
the credentials live on one person's machine.

**The hosted thing you may have heard of** — HCP Terraform, Spacelift, Atlantis —
are services that run this binary *for you*: they hold the state, plan on your
PRs, and gate apply behind approvals. A workflow layer on top. The tool
underneath is still the binary above.

## What Terraform actually is

A program that reads a description of infrastructure, compares it against what
exists, and makes the second match the first.

That sentence contains the whole idea, and the interesting word is *compares*.

This repo already has `infra/localstack-init.sh`, which creates the same queue
and bucket by running AWS CLI commands. Put them side by side:

```bash
# The script says HOW
$AWS sqs create-queue --queue-name "$DLQ"
DLQ_ARN=$($AWS sqs get-queue-attributes --queue-url ... --query 'Attributes.QueueArn')
$AWS sqs create-queue --queue-name "$QUEUE" --attributes "$(build_json "$DLQ_ARN")"
```

```hcl
# Terraform says WHAT
resource "aws_sqs_queue" "events" {
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead_letter.arn
    maxReceiveCount     = 5
  })
}
```

Four differences, and each one is a reason:

1. **The script cannot tell you what it is about to do.** `terraform plan` prints
   every change before anything happens. That is the single most valuable thing
   about the tool, and it is why review of infrastructure changes is possible at
   all.
2. **The script cannot notice drift.** Somebody widens the visibility timeout in
   the console at 2am during an incident. The script re-runs, sees the queue
   exists, and moves on. Terraform's next plan says the timeout differs and
   offers to put it back.
3. **The script cannot delete.** It has no memory of what it made. Terraform has
   state, which is why `terraform destroy` exists and why removing a resource
   from the code removes it from AWS.
4. **The dependency is a reference, not an ordering.** `aws_sqs_queue.dead_letter.arn`
   *is* the statement that the DLQ comes first. Terraform builds a graph from
   those references and parallelises everything unrelated. The script's order is
   the order somebody typed the lines in, and a reordering breaks it silently.

## State: the part that surprises people

Terraform keeps a JSON file recording every resource it created and its current
attributes. Everything else follows from that:

- It is how `plan` knows the difference between "create this" and "change this".
- It is why the file must be **shared and locked** — two people applying at once
  with local state produce two files, each describing half the truth. Hence the
  S3 backend in `versions.tf`.
- It **contains secrets in plaintext**. Any password or key that passes through a
  resource attribute is in there. That is why `storage.tf` creates the secret
  *container* and deliberately does not manage its value.
- Losing it is bad but survivable: `terraform import` adopts existing resources
  back into a fresh state, slowly and by hand.

If somebody asks you one question about Terraform in an interview, it will be
about state.

## The workflow

```bash
terraform init      # download providers, configure the backend. Once per checkout.
terraform fmt       # canonical formatting. Run it before every commit.
terraform validate  # syntax and type checking. No AWS calls, so it is instant.
terraform plan      # what would change. Read this. Every time.
terraform apply     # do it
terraform destroy   # undo it
```

`plan` is the habit worth building. It is not a formality: it is the review step,
and the thing to look for is any line starting with `-` or `-/+`. The second one
means **replace** — destroy then create — and on a database or a load balancer
that is an outage hiding inside a config change.

## The vocabulary, as it appears here

| Concept | Where | What it is |
| --- | --- | --- |
| `provider` | `versions.tf` | The plugin that talks to an API. AWS is one; there are hundreds. |
| `resource` | everywhere | Something Terraform owns and will create, change, or destroy. |
| `data` | `data.tf` | Something it only reads. The VPC here is read, never managed. |
| `variable` | `variables.tf` | Input. Whatever differs between staging and production. |
| `output` | `outputs.tf` | The stack's public interface, for pipelines and other stacks. |
| `locals` | `locals.tf` | Computed values. The `services` map is the shape of this whole design. |
| `for_each` | `ecs.tf`, `iam.tf` | One block, many resources, keyed by name. |
| `dynamic` | `ecs.tf` | A nested block that appears conditionally — the worker's missing load balancer. |
| `lifecycle` | `ecs.tf`, `storage.tf` | Overrides to the default behaviour: `ignore_changes`, `prevent_destroy`. |

### `for_each` rather than `count`

Both repeat a resource. `count` indexes by number, so removing the middle entry
of three renumbers everything after it, and Terraform reads that as *destroy and
recreate* the survivors. `for_each` keys by a string: `aws_iam_role.task["worker"]`
stays that no matter what happens to `ingest`.

Reach for `for_each` by default. `count` is for "zero or one of this".

## What this stack does not create, and why

No VPC, no subnets, no NAT gateways, no RDS, no ElastiCache, no DNS. They are
referenced through variables and data sources.

Not laziness — **blast radius**. A VPC changes rarely and is shared by everything
in the account; an application changes daily and is owned by one team. Putting
them in one state means a routine deploy holds a lock on the network, and a
mistyped `terraform destroy` reaches things this project has never heard of.

Split Terraform by how often things change and by what an accident would take
down. Not by size.

## The ECS parts worth understanding

**Cluster, task definition, service** are three different nouns:

- A **task definition** is immutable. Registering one creates a new *revision*;
  the old revisions stay forever.
- A **service** points at a revision and keeps N copies alive.
- **Deploying** is a service pointed at a new revision. **Rolling back** is
  pointing it at the previous one, which is why an ECS rollback takes a minute
  and not a rebuild.

**The two IAM roles** are the thing to be able to explain:

| | Execution role | Task role |
| --- | --- | --- |
| Used by | The ECS agent | Your process |
| When | Before the container starts | While it runs |
| For | Pulling the image, creating log streams, resolving `secrets` | Every SQS, S3 and Secrets Manager call your code makes |
| Failure looks like | The task never starts; the error is in the ECS event log | The task runs fine, then returns `AccessDenied` |

Getting these backwards is the classic ECS afternoon. If a task will not start,
suspect the execution role; if it starts and then cannot do its job, suspect the
task role.

**One subtlety worth stealing** from `iam.tf`: the API service needs
`s3:PutObject` even though it never uploads anything. Presigning is *local* — the
SDK signs a URL with the caller's credentials and makes no API call — so nothing
fails at signing time. S3 checks the permission when the URL is *used*, so a
missing permission here surfaces as a browser upload failing with nothing at all
in the server logs.

## Why choose Terraform — and when not to

**For it:**

- One tool and one mental model across AWS, Cloudflare, Datadog, GitHub, Postgres
  roles. Most real systems are not in one vendor.
- `plan` makes infrastructure reviewable in a pull request.
- The largest ecosystem of modules and the most people who already know it.
- State enables the whole update/destroy lifecycle a script cannot have.

**Against it, honestly:**

- **The state file is a liability.** It holds secrets, it can be corrupted, and
  recovering from a bad `apply` sometimes means hand-editing it. Nobody enjoys
  their first `terraform state rm`.
- **Drift is inevitable.** People click in consoles during incidents. Terraform
  finds out at the next plan, which might be weeks later.
- **HCL is not a programming language.** Loops and conditionals are awkward on
  purpose. Anything genuinely dynamic fights the tool.
- **Providers lag.** A new AWS feature can be weeks or months from being usable.
- **Slow at scale.** A plan over a large estate refreshes real state and takes
  minutes, every time.

**When something else is the better answer:**

- **CloudFormation / CDK** — AWS only, but no state file to own (AWS keeps it),
  native rollback, and CDK lets you write TypeScript instead of HCL. If you are
  all-in on AWS and the team already writes TypeScript, this is a real contender.
- **Pulumi** — Terraform's model with a real language. Better for genuinely
  dynamic infrastructure; a smaller community.
- **Ansible / Chef** — configuration management. They shape servers that already
  exist. Different problem; sometimes used alongside.
- **The console** — for exploring, and for the one bootstrap resource that has to
  exist before Terraform can store state anywhere.
- **OpenTofu** — the open-source fork, after HashiCorp changed the licence in
  2023. Drop-in compatible today. Worth knowing the name exists, because the
  licence question comes up.

**How I would answer "why Terraform" in an interview:** because infrastructure
changes should be reviewable before they happen, and `plan` is the only reason
that is possible. Everything else — multi-cloud, modules, the ecosystem — is a
consequence. The cost is owning a state file, which is a real cost and worth
naming rather than glossing over.

## Reviewing this code

It has never been applied. Things I would expect a reviewer to catch, and would
want to be asked about:

- There is deliberately no container-level `healthCheck`. ECS runs that command
  *inside* the container, so it needs a shell and wget or curl — which a Go
  binary on `scratch` does not have. The failure looks like a container that
  starts and dies with no application error, which is nothing like its cause.
  The load balancer's check does the same job from outside.
- `readonlyRootFilesystem = true` breaks anything that writes to `/tmp`. Go's
  standard library mostly does not, but a dependency might, and the failure is
  at runtime.
- The egress security group is `0.0.0.0/0`. Narrowing it properly means VPC
  endpoints for SQS, S3, Secrets Manager and ECR — genuinely better, and a
  bigger piece of work than it looks.
- `ARM64` requires the images to be built for it. A `GOARCH=amd64` binary fails
  with an exec format error that looks nothing like a platform mismatch.
- There is no WAF, no rate limiting at the edge, and no DNS record. The
  application rate-limits per merchant, which is not the same as surviving a
  volumetric flood.

## Running it without an account

You cannot apply this against LocalStack Community — ECS is a Pro feature. You
can do everything short of that, which is most of the learning:

```bash
cd infra/terraform
tofu init -backend=false
tofu fmt -check
tofu validate               # syntax and references. No API calls, instant.
```

To go further and get a real plan, point the provider at LocalStack. It needs
`ec2` enabled so the VPC and subnet data sources resolve — the compose file does
that, and nothing in this project uses EC2 at runtime:

```bash
AWS_ENDPOINT_URL=http://localhost:4566 \
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 \
  tofu plan -var-file=/path/to/localstack.tfvars
```

That is the step worth doing. `validate` passed this configuration first time;
`plan` is what proved every argument is one the provider accepts and every data
source resolves.

### What the plan actually found

One real thing, and it is the kind that only shows up at the boundary.

AWS caps load balancer target group names at 32 characters, and
`dispute-router-production-ingest` is **exactly 32**. It plans, it applies, and
it leaves the next person one character of headroom — so a fourth service, or a
longer environment name, fails at apply, from AWS, phrased as a complaint about
the name rather than about its length, after part of the stack already exists.

There is now a `precondition` on the target group that catches it at plan time
and says what to do:

```
Error: Resource precondition failed
  on alb.tf line 58, in resource "aws_lb_target_group" "service":
    │ each.key is "ingest"
    │ local.name is "dispute-router-prod-eu-west"
```

`precondition` is worth knowing generally: it moves a class of failure from
apply — where things are half-created — to plan, where nothing has happened yet.
