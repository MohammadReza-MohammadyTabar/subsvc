FROM python:3.12-alpine

WORKDIR /app

COPY subsvc-ui.py .
COPY subsvc-ui.html ./subsvc-ui.html

EXPOSE 9090

CMD ["python3", "subsvc-ui.py"]