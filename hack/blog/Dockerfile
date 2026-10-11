FROM postgres:16-alpine
COPY serve.sh /usr/local/bin/serve.sh
RUN chmod 0755 /usr/local/bin/serve.sh && mkdir -p /app/uploads
CMD ["/bin/sh", "/usr/local/bin/serve.sh"]
