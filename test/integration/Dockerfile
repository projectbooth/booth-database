# The in-cluster check Job's image (hack/kind-integration.sh): the published client library plus the
# broker stand-in, nothing else. Build context: the repo root.
FROM python:3.13-slim
WORKDIR /check
COPY client /client
RUN pip install --no-cache-dir /client
COPY client/tests/fakecore.py test/integration/in_cluster_check.py ./
ENTRYPOINT ["python", "in_cluster_check.py"]
