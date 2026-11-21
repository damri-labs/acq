# Instinct, acting on behalf of Tal Chaim Damri.
# Run real framework plumbing against the checkout's existing loopback fixture.
import os, pathlib, subprocess, tempfile, time, urllib.parse
root = pathlib.Path(__file__).resolve().parent
source = root.parent.parent / 'server'
with tempfile.TemporaryDirectory(prefix='acq-deepagents-') as tmp:
    temp = pathlib.Path(tmp)
    env = {'PATH': os.environ['PATH'], 'HOME': tmp, 'TMPDIR': tmp,
           'GOTOOLCHAIN': 'local', 'GOPROXY': 'off',
           'GOCACHE': str(temp/'go-cache'), 'GOPATH': str(temp/'go-path'),
           'ACQ_SDK_FIXTURE_READY': str(temp/'ready'),
           'ACQ_LOCAL_READY': str(temp/'ready'),
           'ROOMS_DIR': str(temp/'rooms'), 'STATS_FILE': str(temp/'stats.json'),
           'FEEDBACK_FILE': str(temp/'feedback.json'), 'ANALYTICS_FILE': str(temp/'analytics.json'),
           'LANGSMITH_TRACING': 'false', 'LANGCHAIN_TRACING_V2': 'false',
           'LANGCHAIN_CALLBACKS_BACKGROUND': 'false'}
    binary = temp/'fixture'
    subprocess.run(['go','test','-c','-o',str(binary),'.'], cwd=source, env=env,
                   check=True, timeout=60, capture_output=True)
    server = subprocess.Popen([str(binary),'-test.run=^TestSDKFixture$','-test.timeout=90s'],
                              cwd=temp, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic()+10
        while not (temp/'ready').exists():
            if server.poll() is not None: raise RuntimeError('loopback fixture exited before readiness')
            if time.monotonic()>deadline: raise RuntimeError('loopback fixture readiness deadline')
            time.sleep(.025)
        u = urllib.parse.urlparse((temp/'ready').read_text().strip())
        if u.scheme!='http' or u.hostname!='127.0.0.1' or not u.port or u.path not in ('','/') or u.query or u.fragment:
            raise RuntimeError('fixture address is not a plain loopback URL')
        subprocess.run(['node',str(root/'compat.mjs')],cwd=temp,env=env,check=True,timeout=60)
    finally:
        if server.poll() is None:
            server.terminate()
            try: server.wait(timeout=3)
            except subprocess.TimeoutExpired: server.kill(); server.wait()
