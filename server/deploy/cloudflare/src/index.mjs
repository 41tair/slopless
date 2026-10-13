import { Container, ContainerProxy, getContainer } from "@cloudflare/containers";
import { handleD1Request } from "./d1.mjs";
import { containerRequest } from "./proxy.mjs";

export { ContainerProxy };

export class ServerContainer extends Container {
  defaultPort = 8090;
  requiredPorts = [8090];
  sleepAfter = this.env.CONTAINER_SLEEP_AFTER || "10m";
  pingEndpoint = "localhost/healthz";
  enableInternet = false;
  allowedHosts = ["d1.internal"];
  envVars = {
    D1_TRANSPORT: "binding",
    SERVER_SECURE_COOKIES: "true",
    SERVER_TRUSTED_PROXIES: "0.0.0.0/0,::/0"
  };

  async fetch(request) {
    await this.startAndWaitForPorts({
      ports: [8090],
      cancellationOptions: {
        abort: request.signal,
        instanceGetTimeoutMS: 60_000,
        portReadyTimeoutMS: 60_000
      }
    });
    return this.containerFetch(request, 8090);
  }
}

// This assignment must invoke Container's inherited static setter. A static
// class field would shadow the setter and leave ContainerProxy unregistered.
ServerContainer.outboundByHost = {
  "d1.internal": async (request, env) => handleD1Request(request, env.DB)
};

export default {
  async fetch(request, env) {
    return getContainer(env.SERVER_CONTAINER, "main").fetch(containerRequest(request));
  }
};
