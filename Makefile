SIM := services/simulator

.PHONY: help up down reset migrate seed verify emit psql redis logs

help:
	@echo "up      start postgres (:5433) and redis (:6379)"
	@echo "down    stop them, keeping the data volumes"
	@echo "reset   destroy the volumes and start clean"
	@echo "migrate apply db/migrations"
	@echo "seed    regenerate the dataset (SEED_TRANSACTIONS=n to change size)"
	@echo "verify  assert the money invariants and print a summary"
	@echo "emit    stream signed webhooks at the ingest service"
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

# No local psql client needed; use the one inside the container.
psql:
	docker exec -it dr-postgres psql -U dispute -d dispute_router

redis:
	docker exec -it dr-redis redis-cli

logs:
	docker compose logs -f --tail 100
