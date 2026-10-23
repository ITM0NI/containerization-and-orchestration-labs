FROM alpine/helm:3.17.2 AS helm
FROM registry.k8s.io/kubectl:v1.32.2 AS kubectl
FROM python:3.12-alpine
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl
WORKDIR /lab
COPY gate.py /lab/gate.py
ENV PYTHONDONTWRITEBYTECODE=1
ENTRYPOINT ["python3", "/lab/gate.py"]
