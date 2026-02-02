module.exports = {
  apps: [
    {
      name: "metrics-server",
      script: "/root/load_metrics/metrics_server",
      args: "",
      exec_mode: "fork",
      autorestart: true,
      watch: false,
      max_restarts: 10,
      restart_delay: 5000,
      env: {
        PORT: "9000",
        THRESHOLD_CONFIG: "/root/load_metrics/thresholds.json"
      },
      log_date_format: "YYYY-MM-DD HH:mm:ss",
      error_file: "/root/load_metrics/logs/metrics-server-error.log",
      out_file: "/root/load_metrics/logs/metrics-server-out.log",
      merge_logs: true
    }
  ]
};


