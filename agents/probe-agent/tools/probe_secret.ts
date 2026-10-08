// probe_secret: the declared secret must be in this process's env, and only here.
process.stdout.write(JSON.stringify({
  has_token: (process.env.PROBE_TOKEN ?? "").length > 0,
  uid: process.getuid?.(),
}));
