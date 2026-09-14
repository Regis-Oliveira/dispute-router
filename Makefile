SIM := services/simulator
DASH := apps/dashboard

.PHONY: help up down reset migrate seed verify emit psql redis logs \
        ingest api go-test go-test-integration go-lint tidy dash dash-test \
        dash-build stack attack

help:
	@echo "up      start postgres (:5433) and redis (:6379)"
	@echo "down    stop them, keeping the data volumes"
	@echo "reset   destroy the volumes and start clean"
	@echo "migrate apply db/migrations"
	@echo "seed    regenerate the dataset (SEED_TRANSACTIONS=n to change size)"
	@echo "verify  assert the money invariants and print a summary"
	@echo "emit    stream signed webhooks at the ingest service"
	@echo "rule    send network rulings (won/lost) for represented disputes"
	@echo ""
	@echo "ingest       run the webhook ingest service on :8080"
	@echo "api          run the read API on :8081"
	@echo "worker       run the deadline worker"
	@echo "dash         run the Angular dashboard on :4200"
	@echo "stack        what to run, in which order"
	@echo ""
	@echo "go-test      go test ./... -race"
	@echo "go-test-integration"
	@echo "             the same, plus the tests that flush or drop a database"
	@echo "go-lint      go vet and staticcheck"
	@echo "attack       black-box security suite against the running services"
	@echo "dash-test    unit tests for the dashboard"
	@echo "tidy         resolve Go module dependencies"
	@echo ""
	@echo "aws-init    create the queue, dlq and bucket in localstack"
	@echo "aws-status  queue depths and bucket contents"
	@echo "dlq         show what is on the dead-letter queue"
	@echo "mcp-check   verify the MCP server starts and answers a handshake"
	@echo "tf-check    format and validate the Terraform (needs opentofu)"
	@echo "tf-plan     plan it against localstack, which resolves the data sources"
	@echo "dlq-replay  dry-run a replay back onto the main queue"
	@echo ""
	@echo "psql    open a shell on the database"
	@echo "redis   open redis-cli inside the container"

up:
	docker compose up -d
	@echo "waiting for health..."
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' dr-postgres 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' dr-redis 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@until curl -sf http://localhost:4566/_localstack/health > /dev/null; do sleep 2; done
	@echo "ready"
	@$(MAKE) --no-print-directory aws-init

down:
	docker compose down

reset:
	docker compose down -v
	$(MAKE) up
	$(MAKE) migrate

migrate:
	cd $(SIM) && npm run migrate

seed:
	cd $(SIM) && npm run seed

verify:
	cd $(SIM) && npm run verify

emit:
	cd $(SIM) && npm run emit -- --rate 20

# The same, with deadlines forty seconds out instead of days, so the worker
# claims each one on its next tick. For watching the worker work (open
# http://127.0.0.1:6061/debug/live beside it); not realistic traffic.
# Expect most of it to expire: chargebacks and alerts above the ceiling are
# handed to a person, and forty seconds gives that person ten to act, so the
# worker's next visit - just past the deadline - writes them down as expired.
# That is the policy working, not the worker missing them. Refunds and closes
# are the decisions to watch for.
emit-rush:
	cd $(SIM) && npm run emit -- --rate 120 --rush

# The network coming back with a verdict on a representment. Needs the worker
# to have run first: it is what moves chargebacks to 'represented'.
rule:
	cd $(SIM) && npm run rule -- --count 25

tidy:
	go mod tidy

# Three processes, three terminals. `make emit` in a fourth sends traffic.
stack:
	@echo "terminal 1:  make ingest   # :8080 receives webhooks"
	@echo "terminal 2:  make api      # :8081 serves the dashboard"
	@echo "terminal 3:  make worker   # decides disputes before their deadlines"
	@echo "terminal 4:  make dash     # :4200 the dashboard itself"
	@echo "terminal 5:  make emit     # sends signed disputes at :8080"

# Each service also serves the Go runtime's diagnostics on its own loopback
# port (PPROF_ADDR): profiles, goroutine dumps, and the execution trace that
# shows parallelism per logical processor. Loopback only; nothing is exposed.
ingest:
	PPROF_ADDR=127.0.0.1:6062 go run ./cmd/ingest

api:
	PPROF_ADDR=127.0.0.1:6060 go run ./cmd/api

worker:
	PPROF_ADDR=127.0.0.1:6061 go run ./cmd/worker

# Record ten seconds of a running service and open the trace viewer.
#   make trace P=worker      (api → 6060, worker → 6061, ingest → 6062)
# Generate load in another terminal first: make emit for ingest and api,
# make worker + make emit for the worker.
TRACE_PORT_api := 6060
TRACE_PORT_worker := 6061
TRACE_PORT_ingest := 6062
P ?= worker
trace:
	@mkdir -p .traces
	curl -sf -o .traces/$(P).out 'http://127.0.0.1:$(TRACE_PORT_$(P))/debug/pprof/trace?seconds=10'
	go tool trace .traces/$(P).out

# What the assistant has done and what it cost: outcomes, cost and latency
# distributions, cache share, retrieval method, findings by rule. Free.
agent-report:
	DOTENV_PATH=.env go run ./cmd/agent -report

# The whole flow with real models, measured. Needs ANTHROPIC_API_KEY (and
# VOYAGE_API_KEY for the vector path) in .env. Costs about two cents a
# dispute; N bounds the spend.
#   make flow N=10
N ?= 10
flow:
	@mkdir -p .traces docs/measurements
	DOTENV_PATH=.env go run ./cmd/agent -batch $(N) -trace .traces/agent.out
	DOTENV_PATH=.env go run ./cmd/agent -report | tee docs/measurements/$$(date +%F)-flow.txt
	@echo
	@echo "execution trace of the agent run:  go tool trace .traces/agent.out"

# Stage three drafts in the review queue so the screen can be demonstrated
# without spending anything on a model. Repeatable: run it between takes.
demo-reset:
	docker exec -i dr-postgres psql -U dispute -d dispute_router -q -f - < db/demo/review_queue.sql

go-test:
	go test ./... -race

# The tests behind //go:build integration, plus everything go-test already runs.
#
# They are separated by what they destroy, not by what they touch. A live test
# that only reads is gated by its env var and skips for free, so it belongs in
# the default run. One that flushes a Redis database or creates and drops a
# Postgres one wipes state it did not write, and that has to be asked for by
# name rather than happen to anyone whose .env is populated.
go-test-integration:
	go test ./... -race -tags integration

go-lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

# Black-box security suite: a real HTTP client against the running read API
# (:8081) and ingest service (:8080). The `security` build tag keeps these out
# of `make go-test`; each test skips cleanly when its target is not listening.
# Override ATTACK_API_URL / ATTACK_INGEST_URL to aim elsewhere (distinct from the
# simulator's INGEST_URL, which is a full webhook endpoint). The rate-limit attack drains a
# Redis bucket and is slow, so it stays skipped unless ATTACK_RATE=1.
attack:
	go test -tags security -count=1 -v ./test/security/...

dash:
	cd $(DASH) && npm start

dash-test:
	cd $(DASH) && npm test

dash-build:
	cd $(DASH) && npm run build

# LocalStack: the real AWS APIs, no account. awslocal is the AWS CLI with
# --endpoint-url pre-set, and ships inside the image.
aws-init:
	@./infra/localstack-init.sh

aws-status:
	@echo "queue:"
	@docker exec dr-localstack awslocal sqs get-queue-attributes \
	  --queue-url http://localhost:4566/000000000000/disputes-events \
	  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
	  --query Attributes --output table
	@echo "dead letter queue:"
	@docker exec dr-localstack awslocal sqs get-queue-attributes \
	  --queue-url http://localhost:4566/000000000000/disputes-events-dlq \
	  --attribute-names ApproximateNumberOfMessages \
	  --query Attributes --output table
	@echo "evidence bucket:"
	@docker exec dr-localstack awslocal s3 ls s3://dispute-evidence --recursive --human-readable || true

# Fix the cause before replaying. A message put back into an unfixed failure
# comes straight back, and a loop that looks like work is worse than a queue
# that is visibly stuck.
# Terraform for a real AWS account. Never applied - there is no account behind
# this project - but it does plan cleanly.
tf-check:
	cd infra/terraform && tofu fmt -check -diff && tofu validate

# A plan is the stronger check: it resolves every data source and puts every
# argument through the provider's own validation. Pointed at LocalStack, which
# is enough to compute the graph even though ECS cannot be created there.
#
# The tfvars are generated rather than committed: LocalStack hands out new VPC
# and subnet ids every time its volume is recreated, so a checked-in file would
# be stale the first time somebody ran `make reset`.
tf-plan:
	@./infra/terraform/plan-against-localstack.sh

# The MCP server speaks JSON-RPC on stdin and stdout, so running it by hand just
# blocks - it is meant to be launched by a client (see .mcp.json). The tests run
# a real client against it over the SDK's in-memory transport instead.
mcp-check:
	DATABASE_URL=postgres://dispute:dispute@localhost:5433/dispute_router \
	  go test ./internal/mcpserver/ -v

# One question over the read-only tools, answered by a model in a loop. Costs
# money per question; -dry-run shows the tools and the budget and stops.
#   make ask Q="what is due in the next 24 hours?"
ask:
	go run ./cmd/ask -dry-run
	@echo
	@echo "to spend on it:  go run ./cmd/ask \"$(Q)\""
	@echo "and keep a log:  go run ./cmd/ask -log .traces/ask.jsonl \"$(Q)\""

dlq:
	go run ./cmd/dlq peek

dlq-replay:
	go run ./cmd/dlq replay -dry-run
	@echo
	@echo "that was a dry run. to actually move them:"
	@echo "  go run ./cmd/dlq replay"

# No local psql client needed; use the one inside the container.
psql:
	docker exec -it dr-postgres psql -U dispute -d dispute_router

redis:
	docker exec -it dr-redis redis-cli

logs:
	docker compose logs -f --tail 100
