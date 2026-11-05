/** openTextFile lets the user pick a local file and returns its name and text. */
export function openTextFile(accept: string): Promise<{ name: string; text: string } | undefined> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = accept;
    input.onchange = async () => {
      const f = input.files?.[0];
      resolve(f ? { name: f.name, text: await f.text() } : undefined);
    };
    input.oncancel = () => resolve(undefined);
    input.click();
  });
}

/** downloadText saves text as a file through the browser. */
export function downloadText(name: string, text: string, type = 'application/yaml'): void {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** baseName returns the last path element of a local or Windows path. */
export function baseName(path: string): string {
  return path.split(/[\\/]/).pop() || path;
}

/** openBinaryFile lets the user pick a local file and returns its name and bytes. */
export function openBinaryFile(accept: string): Promise<{ name: string; bytes: Uint8Array } | undefined> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = accept;
    input.onchange = async () => {
      const f = input.files?.[0];
      resolve(f ? { name: f.name, bytes: new Uint8Array(await f.arrayBuffer()) } : undefined);
    };
    input.oncancel = () => resolve(undefined);
    input.click();
  });
}

/** downloadBlob saves a file the server built through the browser. */
export function downloadBlob(name: string, blob: Blob): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
