FROM python:3.12-slim

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

ENV PYTHONUNBUFFERED=1
WORKDIR /app

COPY requirements.txt .
RUN uv pip install --system -r requirements.txt

COPY provisioner.py .
COPY k8s_manager.py .

ENTRYPOINT ["python", "/app/k8s_manager.py"]
