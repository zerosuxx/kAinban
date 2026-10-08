{
  description = "kAinban: Kubernetes-native kanban for AI coding agents";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    # The agent CLIs move fast; take them from master.
    nixpkgs-master.url = "github:nixos/nixpkgs/master";
  };

  outputs = { self, nixpkgs, nixpkgs-master }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems f;
      version = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          master = import nixpkgs-master { inherit system; config.allowUnfree = true; };

          kainban = pkgs.buildGoModule {
            pname = "kainban";
            inherit version;
            src = pkgs.lib.cleanSource self;
            # Update after go.mod changes: build once, copy the "got:" hash.
            vendorHash = "sha256-wyKTsaehLH4xas39/SdxC0NC/FvAuxcPZH6cNSrCUP0=";
            subPackages = [ "cmd/kainban" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" "-X main.appVersion=${version}" ];
            # `k` is a short alias for kainban.
            postInstall = "ln -s kainban $out/bin/k";
            meta.mainProgram = "kainban";
          };

          # Everything the agents (and the orchestrator's TUI flows) run.
          tools = pkgs.buildEnv {
            name = "kainban-tools";
            paths = [
              # agent CLIs
              master.claude-code
              master.codex
              master.github-copilot-cli
              master.antigravity-cli
              pkgs.gh
              pkgs.git
              pkgs.openssh
              # shell and the basics the agents call
              pkgs.bashInteractive
              pkgs.coreutils
              pkgs.findutils
              pkgs.gnugrep
              pkgs.gnused
              pkgs.gawk
              pkgs.diffutils
              pkgs.gnupatch
              pkgs.which
              pkgs.less
              pkgs.ripgrep
              pkgs.fd
              pkgs.jq
              pkgs.curl
              pkgs.cacert
              pkgs.gnutar
              pkgs.gzip
              pkgs.xz
              pkgs.unzip
              pkgs.util-linux # script (Claude token capture), kill
              pkgs.procps
              pkgs.ncurses # terminfo for the TUIs
              pkgs.tmux
              pkgs.tini # PID 1 of the agent pods' shell container (reaps tmux)
              # common project toolchains (latest LTS / stable)
              pkgs.nodejs
              pkgs.python3
            ];
          };

          # uid/gid 1000 "ubuntu", as the chart and agent pods expect.
          nss = pkgs.dockerTools.fakeNss.override {
            extraPasswdLines = [ "ubuntu:x:1000:1000:ubuntu:/home/ubuntu:/bin/bash" ];
            extraGroupLines = [ "ubuntu:x:1000:" ];
          };

          image = pkgs.dockerTools.streamLayeredImage {
            name = "ghcr.io/zerosuxx/kainban";
            tag = version;
            contents = [
              kainban
              tools
              nss
              pkgs.dockerTools.binSh
              pkgs.dockerTools.usrBinEnv
              pkgs.dockerTools.caCertificates
            ];
            fakeRootCommands = ''
              mkdir -p tmp home/ubuntu/.local/state work
              chmod 1777 tmp
              chown -R 1000:1000 home/ubuntu work
            '';
            config = {
              User = "1000:1000";
              WorkingDir = "/home/ubuntu";
              Cmd = [ "sleep" "infinity" ];
              Env = [
                "HOME=/home/ubuntu"
                "USER=ubuntu"
                "SHELL=/bin/bash"
                "PATH=/bin:/usr/bin"
                "LANG=C.UTF-8"
                "TERMINFO_DIRS=/share/terminfo"
                "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
                "NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
                "DISABLE_AUTOUPDATER=1" # claude: the image pins the version
              ];
              Labels = {
                "org.opencontainers.image.source" = "https://github.com/zerosuxx/kAinban";
                "org.opencontainers.image.description" = "kAinban orchestrator and agent image";
              };
            };
          };
        in
        {
          inherit kainban tools image;
          default = kainban;
        });
    };
}
