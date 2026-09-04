# ============================================================================
# csi-scaffold 打包部署全链路
# 用法：make image push deploy example
# ============================================================================

# ---- 可覆盖变量 ----
VERSION    ?= v0.1.0
REGISTRY   ?= registry.example.com/library   # TODO: 改成你的镜像仓库
IMAGE      := $(REGISTRY)/csi-scaffold
NAMESPACE  ?= csi-scaffold
KUBECTL    ?= kubectl
KUSTOMIZE  ?= kustomize

# sidecar 版本集中在 deploy/*.yaml 中管理，升级时改 yaml 即可。

.PHONY: all
all: build

# ---- 本地开发 ----

.PHONY: build
build: ## 编译本地二进制（macOS 下仅用于编译验证）
	go build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/csi-scaffold ./cmd/csi-scaffold

.PHONY: test
test: ## 运行单元测试与静态检查
	go test ./pkg/... -count=1
	go vet ./...

.PHONY: tidy
tidy:
	go mod tidy

# ---- 镜像 ----

.PHONY: image
image: ## 构建镜像（版本号注入二进制）
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: push
push: ## 推送镜像
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

# ---- 集群部署 ----
# kustomize 自动按依赖关系排序应用资源（rbac → csidriver → 工作负载），
# 镜像 tag 通过 kustomize edit set image 注入（改动可在 git diff 中审计）。

.PHONY: deploy
deploy: ## 部署 driver 到集群（先 make push / kind load）
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	cd deploy && $(KUSTOMIZE) edit set image registry.example.com/library/csi-scaffold=$(IMAGE):$(VERSION)
	$(KUBECTL) apply -k deploy/
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/csi-scaffold-controller --timeout=120s
	$(KUBECTL) -n $(NAMESPACE) rollout status daemonset/csi-scaffold-node --timeout=180s

.PHONY: undeploy
undeploy: ## 从集群卸载 driver
	-$(KUBECTL) delete -k deploy/ --ignore-not-found
	-$(KUBECTL) delete namespace $(NAMESPACE) --ignore-not-found

# ---- 示例与验证 ----

.PHONY: example
example: ## 部署 StorageClass + PVC + Pod 示例
	$(KUBECTL) apply -f examples/storageclass.yaml
	$(KUBECTL) apply -f examples/pvc.yaml
	$(KUBECTL) apply -f examples/pod.yaml
	$(KUBECTL) wait --for=condition=Ready pod/csi-scaffold-demo --timeout=120s

.PHONY: verify
verify: ## 验证数据面：写入/读取测试文件
	$(KUBECTL) exec csi-scaffold-demo -- sh -c 'echo hello-csi > /data/test.txt && cat /data/test.txt'

.PHONY: clean-example
clean-example:
	-$(KUBECTL) delete -f examples/pod.yaml --ignore-not-found
	-$(KUBECTL) delete -f examples/pvc.yaml --ignore-not-found
	-$(KUBECTL) delete -f examples/storageclass.yaml --ignore-not-found

.PHONY: logs
logs: ## 查看 driver 日志
	$(KUBECTL) -n $(NAMESPACE) logs -l app=csi-scaffold-controller -c csi-scaffold --tail=100
	$(KUBECTL) -n $(NAMESPACE) logs -l app=csi-scaffold-node -c csi-scaffold --tail=100

.PHONY: help
help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
