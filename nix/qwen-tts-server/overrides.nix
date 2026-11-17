{
  pkgs,
  python,
}: final: prev: let
  inherit (pkgs) lib;
  site = python.sitePackages;

  libDir = pkg: sub: "${final.${pkg}}/${site}/nvidia/${sub}/lib";
  cu13 = pkg: libDir pkg "cu13";
  searchPaths = dirs: lib.concatMapStrings (dir: "\naddAutoPatchelfSearchPath ${dir}") dirs;
  removeFiles = files: lib.concatMapStrings (file: "\nrm -f $out/${site}/nvidia/${file}") files;

  # libcuda.so.1 comes from the host driver at runtime, never from the store.
  # autoPatchelf replaces each wheel's RUNPATH, dropping its $ORIGIN, yet CUDA
  # libraries dlopen siblings by bare name (libcudnn.so.9 loads
  # libcudnn_graph.so.9, libnvrtc loads its builtins), so $ORIGIN goes back in.
  driver = {
    autoPatchelfIgnoreMissingDeps = ["libcuda.so.1"];
    appendRunpaths = ["$ORIGIN" "${pkgs.addDriverRunpath.driverLink}/lib"];
  };

  cuda = name: deps: removed:
    prev.${name}.overrideAttrs (old:
      driver
      // {
        preFixup = (old.preFixup or "") + searchPaths deps;
        postInstall = (old.postInstall or "") + removeFiles removed;
      });

  nvidiaWheels = lib.filter (lib.hasPrefix "nvidia-") (lib.attrNames prev);

  # Multi-node and RDMA transports need MPI, PMIx, OpenSHMEM, UCX, libfabric or
  # rdma-core; a single-GPU process never dlopens them.
  nvshmemPlugins = [
    "nvshmem_bootstrap_mpi.so.3"
    "nvshmem_bootstrap_pmix.so.3"
    "nvshmem_bootstrap_shmem.so.3"
    "nvshmem_transport_gpunetio.so.6"
    "nvshmem_transport_ibdevx.so.6"
    "nvshmem_transport_ibgda.so.6"
    "nvshmem_transport_libfabric.so.6"
    "nvshmem_transport_ucx.so.6"
  ];

  siblingDeps = {
    nvidia-cusolver = map cu13 ["nvidia-cublas" "nvidia-cusparse" "nvidia-nvjitlink"];
  };

  removedFiles = {
    nvidia-cufile = ["cu13/lib/libcufile_rdma.so.1"];
    nvidia-nvshmem-cu13 = map (file: "nvshmem/lib/${file}") nvshmemPlugins;
  };

  torchDeps =
    [
      "$out/${site}/torch/lib"
      (libDir "nvidia-cudnn-cu13" "cudnn")
      (libDir "nvidia-cusparselt-cu13" "cusparselt")
      (libDir "nvidia-nccl-cu13" "nccl")
      (libDir "nvidia-nvshmem-cu13" "nvshmem")
    ]
    ++ map cu13 [
      "nvidia-cuda-runtime"
      "nvidia-cuda-nvrtc"
      "nvidia-cuda-cupti"
      "nvidia-cublas"
      "nvidia-cufft"
      "nvidia-cufile"
      "nvidia-cusolver"
      "nvidia-cusparse"
      "nvidia-nvjitlink"
    ];
in
  lib.genAttrs nvidiaWheels (name: cuda name (siblingDeps.${name} or []) (removedFiles.${name} or []))
  // {
    torch = prev.torch.overrideAttrs (old:
      driver
      // {
        preFixup = (old.preFixup or "") + searchPaths torchDeps;
      });

    torchaudio = prev.torchaudio.overrideAttrs (old:
      driver
      // {
        preFixup = (old.preFixup or "") + searchPaths ["${final.torch}/${site}/torch/lib" (cu13 "nvidia-cuda-runtime")];
      });

    triton = prev.triton.overrideAttrs (old:
      driver
      // {
        preFixup = (old.preFixup or "") + searchPaths ["$out/${site}/triton/_C"];
      });

    numba = prev.numba.overrideAttrs (old: {
      buildInputs = (old.buildInputs or []) ++ [pkgs.onetbb];
    });

    # Wheel selection ties on the py2.py3 tag and picks the pure wheel, which has
    # no bundled libsndfile and finds none on NixOS.
    soundfile = prev.soundfile.overrideAttrs (old: {
      postInstall =
        (old.postInstall or "")
        + ''
          substituteInPlace $out/${site}/soundfile.py \
            --replace-fail "_explicit_libname = 'libsndfile.so'" \
            "_explicit_libname = '${lib.getLib pkgs.libsndfile}/lib/libsndfile.so'"
        '';
    });

    # The sox sdist ships only setup.py, so its setuptools build backend is undeclared.
    sox = prev.sox.overrideAttrs (old: {
      nativeBuildInputs = (old.nativeBuildInputs or []) ++ final.resolveBuildSystem {setuptools = [];};
    });
  }
