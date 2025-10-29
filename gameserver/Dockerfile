FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

# Install runtime dependencies that are commonly required by native Linux game binaries.
RUN apt-get update && apt-get install -y \
    ca-certificates \
    libstdc++6 \
    libgcc-s1 \
    libcurl4 \
    openssl \
    libfontconfig1 \
    fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# Move Server Binary and PCK files into the container
COPY OpenChamp.console.x86_64 ./
COPY *.pck ./
RUN chmod +x OpenChamp.console.x86_64

# Expose a single default port. The host mapping is done when running the container by the PortManager.
EXPOSE 7000

ENTRYPOINT ["./OpenChamp.console.x86_64"]