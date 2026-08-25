BINARY   := baton
PREFIX   ?= /usr/local
BINDIR   ?= $(PREFIX)/bin
ETCDIR   ?= /etc/baton
SYSDIR   ?= /etc/systemd/system

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) .

.PHONY: install
install: build
	install -d $(DESTDIR)$(BINDIR) $(DESTDIR)$(ETCDIR)
	install -m 0755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	@if [ ! -f $(DESTDIR)$(ETCDIR)/baton.env ]; then \
		install -m 0600 .env.example $(DESTDIR)$(ETCDIR)/baton.env; \
		echo "Created $(ETCDIR)/baton.env - edit it to set NAVIDROME_USER/PASSWORD"; \
	else \
		echo "$(ETCDIR)/baton.env already exists; not overwriting"; \
	fi
	install -m 0644 packaging/baton.service $(DESTDIR)$(SYSDIR)/baton.service
	systemctl daemon-reload || true
	systemctl enable baton || true
	systemctl start baton || true

.PHONY: uninstall
uninstall:
	systemctl stop baton || true
	systemctl disable baton || true
	rm -f $(DESTDIR)$(SYSDIR)/baton.service
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY)
	rm -rf $(DESTDIR)$(ETCDIR)
	systemctl daemon-reload || true

.PHONY: dist
dist:
	@mkdir -p dist
	@for t in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64; do \
		os=$${t%%-*}; arch=$${t##*-}; \
		ext=$$([ "$$os" = "windows" ] && echo .exe || echo ""); \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" -o dist/baton-$$t$$ext .; \
		echo "built dist/baton-$$t$$ext"; \
	done
	@cd dist && sha256sum baton-* > SHA256SUMS
	@echo "checksums in dist/SHA256SUMS"

.PHONY: docker
docker:
	docker compose up -d --build

.PHONY: clean
clean:
	rm -f $(BINARY)

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...
