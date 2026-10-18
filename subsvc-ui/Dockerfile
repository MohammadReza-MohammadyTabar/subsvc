FROM python:3.12-alpine

WORKDIR /app

COPY subsvc-ui.py .
COPY subsvc-ui.html ./subsvc-ui.html

RUN adduser -D -H -u 10001 subsvcui
USER 10001

# SUBSVC_UI_BACKEND, SUBSVC_UI_PORT, SUBSVC_AUTH_TOKEN, SUBSVC_UI_OUTPUT_NAME,
# SUBSVC_UI_OUTPUT_NUMBERED are read from the environment (see docker-compose.yml).
ENV PYTHONUNBUFFERED=1 SUBSVC_UI_PORT=9090
EXPOSE 9090

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD python3 -c "import os,urllib.request; urllib.request.urlopen('http://127.0.0.1:%s/' % os.environ.get('SUBSVC_UI_PORT','9090'), timeout=3)"

CMD ["python3", "subsvc-ui.py"]
