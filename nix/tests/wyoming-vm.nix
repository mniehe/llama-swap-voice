# The Wyoming listener seen from another host. The three servers differ from
# `allowed` in one control each, so each refusal names its cause.
{
  pkgs,
  module,
}: let
  fixture = ../../internal/audio/testdata/speech.wav;
  clip = "/srv/clips/sample.wav";
  wyomingPort = 10310;

  # Not the client's address, so `denied` keeps an allow list of the same
  # shape as `allowed`.
  otherAddress = "192.0.2.10";

  wyomingClient = pkgs.writeShellScriptBin "wyoming-client" ''
    exec ${pkgs.python3}/bin/python3 -I ${./wyoming-client.py} "$@"
  '';

  server = {
    openFirewall,
    allowClient,
  }: {nodes, ...}: {
    imports = [module];

    systemd.tmpfiles.rules = [
      "d /srv/clips 0755 root root -"
      "C ${clip} - - - - ${fixture}"
    ];

    networking.firewall.enable = true;

    # describe needs no upstream, so none runs.
    services.llama-swap-voice = {
      enable = true;
      llamaSwap.enable = false;
      defaultVoice = "sample";
      voices.sample.clip = clip;
      ipAddressAllow =
        if allowClient
        then [nodes.client.networking.primaryIPAddress]
        else [otherAddress];
      wyoming = {
        enable = true;
        host = "0.0.0.0";
        port = wyomingPort;
        inherit openFirewall;
      };
    };

    environment.systemPackages = [wyomingClient];
  };
in
  pkgs.testers.runNixOSTest {
    name = "llama-swap-voice-wyoming";

    nodes = {
      client.environment.systemPackages = [wyomingClient];
      allowed = server {
        openFirewall = true;
        allowClient = true;
      };
      denied = server {
        openFirewall = true;
        allowClient = false;
      };
      closed = server {
        openFirewall = false;
        allowClient = true;
      };
    };

    testScript = {nodes, ...}: ''
      import json

      PORT = ${toString wyomingPort}
      ADDRESSES = {
          "allowed": "${nodes.allowed.networking.primaryIPAddress}",
          "denied": "${nodes.denied.networking.primaryIPAddress}",
          "closed": "${nodes.closed.networking.primaryIPAddress}",
      }
      # wyoming-client.py's status when it cannot connect. A listener that
      # accepts and then stays silent gives 4 instead.
      EXIT_UNREACHABLE = 3
      VOICES = [{"name": "sample", "description": "sample (default)"}]


      def describe_from_client(server):
          return client.execute(f"wyoming-client --host {ADDRESSES[server.name]} --port {PORT} --timeout 5 describe")


      def unreachable(server):
          status, output = describe_from_client(server)
          print(f"{server.name}: wyoming-client status {status}, output {output.strip()}")
          assert status == EXIT_UNREACHABLE, f"{server.name}: status {status}, output {output!r}"
          assert "unreachable" in json.loads(output), output


      start_all()
      client.wait_for_unit("multi-user.target")
      for server in (allowed, denied, closed):
          server.wait_for_unit("llama-swap-voice.service")
          server.wait_for_open_port(8920)

      with subtest("every server answers describe over loopback, so its listener runs"):
          for server in (allowed, denied, closed):
              info = json.loads(server.succeed(f"wyoming-client --port {PORT} describe"))
              assert info["tts_voices"] == VOICES, f"{server.name}: {info}"

      with subtest("allowed: an open port and the client in ipAddressAllow give the client info"):
          status, output = describe_from_client(allowed)
          assert status == 0, f"status {status}, output {output!r}"
          assert json.loads(output)["tts_voices"] == VOICES, output

      with subtest("denied: only ipAddressAllow differs from allowed, so the unit's address filter stops the TCP handshake"):
          unreachable(denied)

      with subtest("closed: only openFirewall differs from allowed, so the firewall stops the TCP handshake"):
          unreachable(closed)
    '';
  }
