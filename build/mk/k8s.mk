KUBECTL_VER = 1.28.6
kubectl/install: ## Install kubectl.
	@kubectl version --client | grep $(KUBECTL_VER); \
		if [ $$? -eq 1 ]; then \
			curl -LOsS https://storage.googleapis.com/kubernetes-release/release/v$(KUBECTL_VER)/bin/linux/amd64/kubectl; \
			chmod +x kubectl; \
			sudo mv kubectl /usr/local/bin; \
			kubectl version --client; \
		else \
			echo "${GREEN} [*] Kubectl already installed ${RESET}"; \
		fi

KUBECTX_VER = 0.9.5
KUBECTX_URL= https://github.com/ahmetb/kubectx/releases/download/v$(KUBECTX_VER)/kubectx_v$(KUBECTX_VER)_linux_x86_64.tar.gz
k8s/install/kubectx: ## Install kubectx
	wget -N $(KUBECTX_URL) -O /tmp/kubectx.tar.gz
	tar zxvf /tmp/kubectx.tar.gz -C /tmp
	sudo mv /tmp/kubectx /usr/local/bin/
	chmod +x /usr/local/bin/kubectx
	kubectx

KUBENS_VER = 0.9.5
KUBENS_URL = https://github.com/ahmetb/kubectx/releases/download/v$(KUBENS_VER)/kubens_v$(KUBENS_VER)_linux_x86_64.tar.gz
k8s/install/kubens: ## Install Kubens
	wget -N $(KUBENS_URL) -O /tmp/kubens.tar.gz
	tar zxvf /tmp/kubens.tar.gz -C /tmp
	sudo mv /tmp/kubens /usr/local/bin/
	chmod +x /usr/local/bin/kubens
	kubens

KUBE_CAPACITY_VER = 0.7.1
k8s/install/kube-capacity: ## Install kube-capacity
	wget https://github.com/robscott/kube-capacity/releases/download/$(KUBE_CAPACITY_VER)/kube-capacity_$(KUBE_CAPACITY_VER)_Linux_x86_64.tar.gz -P /tmp
	cd /tmp \
		&& tar zxvf kube-capacity_$(KUBE_CAPACITY_VER)_Linux_x86_64.tar.gz \
		&& sudo mv kube-capacity /usr/local/bin \
		&& kube-capacity

k8s-prepare:	k8s-kubectl/install k8s-kube-capacity k8s-minikube-start ## Install minikube, kubectl, kube-capacity and start a cluster

k8s/pf/nsq: ## Port forward NSQ admin service.
	kubectl port-forward svc/$(SAFERWALL_RELEASE_NAME)-nsqadmin 4171:4171 --address='0.0.0.0' &
	while true ; do nc -vz 127.0.0.1 4171 ; sleep 5 ; done

k8s/pf/grafana: ## Port forward grafana dashboard service.
	kubectl port-forward --namespace prometheus \
		deployment/prometheus-grafana 3000:3000 --address='0.0.0.0' &
	while true ; do nc -vz 127.0.0.1 3000 ; sleep 5 ; done

k8s/pf/couchbase: ## Port forward couchbase ui service.
	kubectl port-forward svc/couchbase-cluster-ui 8091:8091 --address='0.0.0.0' &
	while true ; do nc -vz 127.0.0.1 8091 ; sleep 5 ; done

k8s/pf/hubble: ## Port forward hubble ui service.
	kubectl port-forward -n kube-system svc/hubble-ui 12000:80 --address='0.0.0.0' &
	while true ; do nc -vz 127.0.0.1 12000 ; sleep 5 ; done

k8s-pf: ## Port forward all services.
	make k8s/pf/nsq &
	make k8s/pf/couchbase &
	make k8s/pf/grafana &
	make k8s/pf/kibana &

k8s-delete-all-objects: ## Delete all objects
	kubectl delete "$(kubectl api-resources --namespaced=true --verbs=delete -o name | tr "\n" "," | sed -e 's/,$//')" --all

k8s-dump-tls-secrets: ## Dump TLS secrets
	sudo apt install jq -y
	$(eval HELM_RELEASE_NAME := $(shell sudo helm ls --filter saferwall --output json | jq '.[0].name' | tr -d '"'))
	$(eval HELM_SECRET_TLS_NAME := $(HELM_RELEASE_NAME)-tls)
	kubectl get secret $(HELM_SECRET_TLS_NAME) -o jsonpath="{.data['ca\.crt']}" | base64 --decode  >> ca.crt
	kubectl get secret $(HELM_SECRET_TLS_NAME) -o jsonpath="{.data['tls\.crt']}" | base64 --decode  >> tls.crt
	kubectl get secret $(HELM_SECRET_TLS_NAME) -o jsonpath="{.data['tls\.key']}" | base64 --decode  >> tls.key
	openssl pkcs12 -export -out saferwall.p12 -inkey tls.key -in tls.crt -certfile ca.crt

CERT_MANAGER_VER=1.16.1
k8s/init-cert-manager: ## Init cert-manager
	# Install the chart.
	helm install cert-manager jetstack/cert-manager \
		--namespace cert-manager \
		--create-namespace \
		--version v$(CERT_MANAGER_VER) \
		--set crds.enabled=true
	# Verify the installation.
	kubectl wait --namespace cert-manager \
	--for=condition=ready pod \
	--selector=app.kubernetes.io/instance=cert-manager \
	--timeout=90s

METALLB_VERSION=0.14.18
k8s/install-metallb: ## Install Metallb helm chart.
	# Create namespace.
	kubectl apply -f $(ROOT_DIR)/build/k8s/metallb/namespace.yaml
	# Install the chart.
	helm install metallb metallb/metallb \
		--namespace metallb-system \
		--version v$(METALLB_VERSION)
	# Verify the installation.
	kubectl wait --namespace metallb-system \
	--for=condition=ready pod \
	--selector=app.kubernetes.io/instance=metallb \
	--timeout=90s
	# Create an IP adress pool and L2 advertisement.
	kubectl apply -f $(ROOT_DIR)/build/k8s/metallb/pool.yaml

KUBE_PROMETHEUS_STACK=69.2.0
k8s/install/kube-prometheus-stack: ## Install Kube Prometheus Stack.
	helm install prometheus prometheus-community/kube-prometheus-stack \
		--version v$(KUBE_PROMETHEUS_STACK) \
		--create-namespace \
		--namespace prometheus

LOKI= 5.42.1
k8s/install-loki: ## Install Loki.
	helm install loki grafana/loki \
		--version v$(LOKI) \
		-f $(ROOT_DIR)/build/k8s/loki-values.yaml \
		--namespace monitoring \
		--create-namespace

PROMTAIL= 6.15.5
k8s/install-promtail: ## Install Loki Distributed.
	helm install promtail grafana/promtail \
		--version v$(PROMTAIL) \
		--namespace monitoring

k8s/install/nfs-client: ## Install nfs subdir external provisioner
	helm install nfs-subdir-external-provisioner nfs-subdir-external-provisioner/nfs-subdir-external-provisioner \
    	--set nfs.server=$(NFS_SERVER) \
    	--set nfs.path=$(NFS_PATH) \
		--set storageClass.name=nfs

COUCHBASE_OPERATOR=2.70.0
k8s/install-couchbase-crds: ## Install couchbase operator CRDs.
	kubectl apply -f https://raw.githubusercontent.com/couchbase-partners/helm-charts/couchbase-operator-$(COUCHBASE_OPERATOR)/charts/couchbase-operator/crds/couchbase.crds.yaml

k8s/rm-cert-manager-crds: ## Delete cert-manager crd objects.
	kubectl get crd | grep cert-manager | xargs --no-run-if-empty kubectl delete crd
	kubectl delete namespace cert-manager

k8s/rm-couchbase-crds: ## Delete couchbase crd objects.
	kubectl get crd | grep couchbase | xargs --no-run-if-empty kubectl delete crd

k8s-events: ## Get Kubernetes cluster events.
	kubectl get events --sort-by='.metadata.creationTimestamp'

k8s/delete-terminating-pods: ## Force delete pods stuck at `Terminating` status
	for p in $$(kubectl get pods | grep Terminating | awk '{print $$1}'); \
	 do kubectl delete pod $$p --grace-period=0 --force;done

k8s/delete-erroring-pods: ## Force delete pods stuck at `Error` status
	for p in $$(kubectl get pods | grep Error | awk '{print $$1}'); \
	 do kubectl delete pod $$p --grace-period=0 --force;done

k8s/delete-evicted-pods: ## Clean up all evicted pods
	kubectl get pods | grep Evicted | awk '{print $$1}' | xargs kubectl delete pod

k8s/get-restarted-pods: ## Get pods who restarted at least once.
	kubectl get pods -A | awk '$$5>0'

k8s/restart/deployment: ## Fetch new container images by restarting all `saferwall` pods.
	kubectl get deployments | grep saferwall | awk '{ print $$1}' | xargs kubectl rollout restart deployments

k8s/restart/deployment/multiav: ## Fetch new multi-av images by restarting all `multiav` pods.
	kubectl get deployments | grep multiav | awk '{ print $$1}' | xargs kubectl rollout restart deployments
