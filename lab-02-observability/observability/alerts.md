# Алерты: установка и проверка

Команды выполняются из каталога `lab-02-observability`.
Предполагается существующий release `monitoring` (chart 91.8.2)
и запущенный API с ServiceMonitor.

## Установка

```bash
export KUBECONFIG="/home/david/.kube/itmo-observability.yaml"

docker build -t lab2-alert-receiver:dev ./alert-receiver
kind load docker-image lab2-alert-receiver:dev --name itmo-observability

helm upgrade --install lab2-alerts ./charts/lab2-alerts \
  --kube-context kind-itmo-observability \
  --namespace monitoring \
  --wait --timeout 2m

helm upgrade monitoring \
  oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack \
  --version 91.8.2 \
  --kube-context kind-itmo-observability \
  --namespace monitoring \
  --reuse-values \
  --values ./observability/monitoring-alerts-values.yaml \
  --wait --timeout 5m
```

`--reuse-values` сохраняет настройки ресурсов, PVC и Grafana существующего
release. Это overlay, а не конфигурация для пустого кластера.
Chart `lab2-alerts` ставит получатель и PrometheusRule;
Alertmanager обновляется в том же release, где уже работает мониторинг.

## Окна наблюдения

Каждый port-forward — отдельный терминал. Уже запущенный второй раз не нужен.

```bash
kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n monitoring port-forward service/monitoring-prometheus 19090:9090

kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n monitoring port-forward service/monitoring-alertmanager 19093:9093
```

Prometheus: `http://127.0.0.1:19090/alerts` — сначала проверить наличие трех
правил, без ошибок вычисления. Alertmanager: `http://127.0.0.1:19093`.

В отдельном терминале видны настоящие уведомления, а не только список алертов:

```bash
kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n monitoring logs deployment/lab2-alert-receiver -f
```

## Ошибки и задержки

Оставить работающим port-forward API `18080:8080`, затем:

```bash
bash scripts/trigger-alerts.sh 240
```

Скрипт последовательно делает пачки `/fail` и `/slow` около четырех минут.
Ограничены время, размер пачек и параллельность; Ctrl+C прекращает нагрузку.
Умышленно не запускает две пачки `/load` одновременно: API разрешает одну.

В Prometheus два правила должны сначала стать pending, затем firing.
После этого они появятся в Alertmanager, а webhook получит JSON
с `status="firing"`. Сделать скрин Alertmanager и сохранить значимые строки
лога получателя. По окончании генерации окно rate еще содержит старые запросы:
resolved не обязан появиться мгновенно.

## Недоступность: отдельный эксперимент

Только на этом локальном стенде, после завершения смешанной нагрузки.
Сначала убедиться, что нормальное число реплик — 1:

```bash
kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n lab2 get deployment lab2-api
```

Временно убрать готовые реплики:

```bash
kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n lab2 scale deployment/lab2-api --replicas=0
```

Примерно через 1–1.5 минуты `Lab2APIUnavailable` должен стать firing.
Сделать скрин Alertmanager и сохранить соответствующее уведомление.

**Обязательно восстановить Deployment**, даже если проверка не удалась:

```bash
kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n lab2 scale deployment/lab2-api --replicas=1

kubectl --kubeconfig /home/david/.kube/itmo-observability.yaml \
  -n lab2 rollout status deployment/lab2-api --timeout=120s
```

После удаления прежнего Pod завершится port-forward API. Для дальнейших
запросов запустить его заново. Jaeger не перезапускается этим экспериментом,
но его in-memory хранилище все равно не переживает собственный рестарт.

## Локальные проверки правил

```bash
docker run --rm --network none \
  --mount type=bind,source="$PWD",target=/work,readonly \
  --workdir /work/observability \
  --entrypoint /bin/promtool \
  quay.io/prometheus/prometheus:v3.15.0-distroless \
  test rules alert-rules.test.yaml

helm lint ./charts/lab2-alerts --strict
```

Unit-тесты проверяют окно ожидания, устойчивое нарушение, короткий всплеск,
здоровый трафик, малый объем запросов и отсутствие доступных реплик/данных.
Они не заменяют проверку настоящей доставки Prometheus → Alertmanager → webhook.
