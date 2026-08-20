import { useEffect, useState } from "react";
import { aoBridge } from "../lib/bridge";

export type DevSandboxInfo = {
	isDevSandbox: boolean;
	dataDir: string | null;
};

const initial: DevSandboxInfo = { isDevSandbox: false, dataDir: null };

// Surfaces whether this Electron process is isolated to ~/.ao/dev (unpackaged
// `npm run dev`) instead of the real ~/.ao install, so the UI never leaves a
// developer silently looking at sandbox data while believing it's the real
// install. See CLAUDE.md "IMPORTANT PRODUCT RULE".
export function useDevSandboxInfo(): DevSandboxInfo {
	const [info, setInfo] = useState<DevSandboxInfo>(initial);
	useEffect(() => {
		let cancelled = false;
		void aoBridge.daemon.getEnvInfo().then((result) => {
			if (!cancelled) setInfo(result);
		});
		return () => {
			cancelled = true;
		};
	}, []);
	return info;
}
