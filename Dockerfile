FROM golang:alpine as builder

# Set necessary environmet variables needed for our image
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

# Move to working directory /build
WORKDIR /build

# Copy and download dependency using go mod
COPY go.mod .
COPY go.sum .
RUN go mod download

# Copy the code into the container
COPY . .

# Build the application
RUN go build -o exporter_exporter_extended_labels  .


FROM alpine

# Move to /dist directory as the place for resulting binary folder
WORKDIR /dist

# Copy binary from build to main folder
COPY --from=builder /build/exporter_exporter_extended_labels .

# Export necessary port
EXPOSE 9999

WORKDIR /etc/exporter_exporter_extended_labels
# Command to run when starting the container
CMD ["/dist/exporter_exporter_extended_labels", "-config.file=expexp_extended_labels.yaml"]
