#!/usr/bin/env python3
"""Create an offline local release archive, checksums and provenance (no upload)."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile


def require_published_tag(root, version, commit):
    tag = 'v' + version
    ref = 'refs/tags/' + tag
    try:
        kind = subprocess.check_output(['git', 'cat-file', '-t', ref], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
        target = subprocess.check_output(['git', 'rev-parse', ref + '^{commit}'], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
    except subprocess.CalledProcessError as err:
        raise ValueError('local publication requires a matching annotated tag ' + tag) from err
    if kind != 'tag' or target != commit:
        raise ValueError('local publication requires a matching annotated tag ' + tag)
    return tag


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--version', required=True)
    p.add_argument('--output', required=True, type=Path)
    p.add_argument('--development', action='store_true', help='allow dirty source for isolated candidate testing only')
    p.add_argument('--publish-local', action='store_true', help='publish the verified offline artifact for a matching annotated local tag; no upload')
    a = p.parse_args()
    if not a.development and not a.publish_local:
        p.error('formal packages require --publish-local and a matching annotated tag')
    if a.publish_local and (a.development or not re.fullmatch(r'\d+\.\d+\.\d+', a.version)):
        p.error('local publication requires a final version and clean formal source')
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[a-z0-9.]+)?', a.version):
        p.error('simple semantic version required')
    if a.development and '-' not in a.version:
        p.error('development packages require a prerelease version')
    if not a.output.is_absolute() or a.output.exists():
        p.error('new absolute output directory required')
    root = Path(__file__).resolve().parents[1]
    if a.output.is_relative_to(root):
        p.error('release output must be outside the source checkout')
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True).strip())
    if dirty and not a.development:
        p.error('formal packages require clean committed source; use --development only for candidate checks')
    source_tag = require_published_tag(root, a.version, commit) if a.publish_local else None
    tag_object = subprocess.check_output(['git', 'rev-parse', 'refs/tags/' + source_tag], cwd=root, text=True).strip() if source_tag else None
    arch = subprocess.check_output(['go', 'env', 'GOARCH'], cwd=root, text=True).strip()
    epoch = int(subprocess.check_output(['git', 'show', '-s', '--format=%ct', 'HEAD'], cwd=root, text=True).strip())
    for script in ('test_install_local.py', 'test_gateway_migrate.py'):
        subprocess.run([sys.executable, '-I', '-B', str(root / 'tools' / script)], check=True, cwd=root)
    a.output.mkdir(mode=0o700)
    name = f'recalldock_{a.version}_linux_{arch}'
    package = a.output / name
    args = [sys.executable, '-I', '-B', str(root / 'tools/package-host-plugins.py'), '--version', a.version, '--output', str(package)]
    if not a.development:
        args += ['--require-clean']
    subprocess.run(args, check=True, cwd=root)
    manifest_path = package / 'release.json'
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
    if manifest['source_commit'] != commit or manifest['source_dirty'] != dirty:
        raise ValueError('package source identity differs from release build')
    manifest.update(development=a.development, published=a.publish_local)
    if source_tag:
        manifest.update(source_tag=source_tag, source_tag_object=tag_object)
    manifest_path.write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n', encoding='utf-8')
    subprocess.run([sys.executable, '-I', '-B', str(root/'tools/smoke-install.py'), '--release', str(package), '--work-dir', str(a.output), '--report', str(a.output/'install-validation.json')], check=True, cwd=root)
    for script, report_name in [('smoke-obsidian-graph.py','obsidian-validation.json'),('eval-retrieval.py','retrieval-validation.json'),('smoke-candidate-evidence.py','candidate-evidence-validation.json'),('smoke-project-scope.py','project-scope-validation.json'),('smoke-project-identity.py','project-identity-validation.json')]:
        subprocess.run([sys.executable,'-I','-B',str(root/'tools'/script),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output),'--report',str(a.output/report_name)],check=True,cwd=root)
    # Shared-vocabulary / negative-query fixture: lexical disambiguation must keep
    # precision at 1.0 and must never fabricate results for absent tokens.
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/eval-retrieval.py'),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output),'--report',str(a.output/'retrieval-validation-v2.json'),'--fixture',str(root/'testdata/retrieval/bilingual-v2.json')],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/eval-retrieval.py'),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output),'--report',str(a.output/'retrieval-validation-v3.json'),'--fixture',str(root/'testdata/retrieval/bilingual-v3.json')],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/eval-gateway.py'),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output),'--report',str(a.output/'gateway-context-validation.json')],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/eval-admission.py'),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output),'--report',str(a.output/'admission-validation.json'),'--require-pass'],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/smoke-operations.py'),'--binary',str(package/'runtime/recalldock'),'--report',str(a.output/'operations-validation.json')],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/smoke-gateway.py'),'--binary',str(package/'runtime/recalldock'),'--report',str(a.output/'gateway-validation.json')],check=True,cwd=root)
    subprocess.run([sys.executable,'-I','-B',str(root/'tools/smoke-host-adapter.py'),'--binary',str(package/'runtime/recalldock'),'--work-dir',str(a.output)],check=True,cwd=root)
    if not a.development and (subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip()!=commit or subprocess.check_output(['git','status','--porcelain'],cwd=root,text=True).strip()):
        raise ValueError('source changed during release build; preserve outputs for inspection and rebuild from a clean commit')
    if source_tag:
        require_published_tag(root, a.version, commit)
        if subprocess.check_output(['git', 'rev-parse', 'refs/tags/' + source_tag], cwd=root, text=True).strip() != tag_object:
            raise ValueError('annotated tag changed during release build')
    archive = a.output / (name + '.tar.gz')
    with archive.open('xb') as raw:
        with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as gz:
            with tarfile.open(mode='w', fileobj=gz, format=tarfile.PAX_FORMAT) as tar:
                for path in [package] + sorted(package.rglob('*')):
                    info = tar.gettarinfo(str(path), arcname=str(path.relative_to(a.output)))
                    if info.issym() or info.islnk():
                        raise ValueError('release package must not contain symlinks')
                    info.uid = info.gid = 0
                    info.uname = info.gname = ''
                    info.mtime = epoch
                    if path.is_file():
                        with path.open('rb') as f:
                            tar.addfile(info, f)
                    else:
                        tar.addfile(info)
    sha = hashlib.sha256(archive.read_bytes()).hexdigest()
    (a.output / 'checksums.txt').write_text(sha + '  ' + archive.name + '\n')
    report: dict = dict(format=1, version=a.version, source_commit=commit, source_dirty=dirty,
                  development=a.development, platform='linux', architecture=arch,
                  archive=archive.name, sha256=sha, published=a.publish_local)
    if source_tag is not None:
        report['source_tag'] = source_tag
        report['source_tag_object'] = tag_object
        report['publication_target'] = 'local-offline-artifact'
    (a.output / 'build.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
