SIM := services/simulator
DASH := apps/dashboard

.PHONY: help up down reset migrate seed verify emit psql redis logs \
        ingest api go-test go-lint tidy dash dash-test dash-build stack

help:
	@echo "up      start postgres (:5433) and redis (:6379)"
	@echo "down    stop them, keeping the data volumes"
	@echo "reset   destroy the volumes and start clean"
	@echo "migrate apply db/migrations"
	@echo "seed    regenerate the dataset (SEED_TRANSACTIONS=n to change size)"
	@echo "verify  assert the money invariants and print a summary"
	@echo "emit    stream signed webhooks at the ingest service"
	@echo ""
	@echo "ingest       run the webhook ingest service on :8080"
	@echo "api          run the read API on :8081"
	@echo "dash         run the Angular dashboard on :4200"
	@echo "stack        what to run, in which order"
	@echo ""
	@echo "go-test      go test ./... -race"
	@echo "dash-test    unit tests for the dashboard"
	@echo "tidy         resolve Go module dependencies"
	@echo ""
	@echo "psql    open a shell on the database"
	@echo "redis   open redis-cli inside the container"

up:
	docker compose up -d
	@echo "waiting for health..."
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' dr-postgres 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' dr-redis 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@echo "ready"

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

tidy:
	go mod tidy

# Three processes, three terminals. `make emit` in a fourth sends traffic.
stack:
	@echo "terminal 1:  make ingest   # :8080 receives webhooks"
	@echo "terminal 2:  make api      # :8081 serves the dashboard"
	@echo "terminal 3:  make dash     # :4200 the dashboard itself"
	@echo "terminal 4:  make emit     # sends signed disputes at :8080"

ingest:
	go run ./cmd/ingest

api:
	go run ./cmd/api

go-test:
	go test ./... -race

go-lint:
	go vet ./...

dash:
	cd $(DASH) && npm start

dash-test:
	cd $(DASH) && npm test

dash-build:
	cd $(DASH) && npm run build

# No local psql client needed; use the one inside the container.
psql:
	docker exec -it dr-postgres psql -U dispute -d dispute_router

redis:
	docker exec -it dr-redis redis-cli

logs:
	docker compose logs -f --tail 100
