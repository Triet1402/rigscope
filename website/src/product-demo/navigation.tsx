import {useMemo, useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent} from 'react';

const subscribe = (callback:() => void) => {
  window.addEventListener('hashchange', callback);
  return () => window.removeEventListener('hashchange', callback);
};
const getRoute = () => window.location.hash.slice(1) || '/machines/workstation';

export function useRoute() {
  return useSyncExternalStore(subscribe, getRoute, () => '/machines/workstation');
}
export function usePathname() { return useRoute().split('?')[0].split('#')[0]; }
export function useSearchParams() {
  const route = useRoute();
  return useMemo(() => new URLSearchParams(route.split('?')[1] ?? ''), [route]);
}
const router = {
  push(href:string) { window.location.hash = href; },
  replace(href:string, _options?:{scroll?:boolean}) {
    window.history.replaceState(null, '', '#' + href);
    window.dispatchEvent(new HashChangeEvent('hashchange'));
  },
};
export function useRouter() { return router; }

export default function DemoLink({href, onClick, children, ...props}:AnchorHTMLAttributes<HTMLAnchorElement> & {href:string}) {
  const internal = href.startsWith('/');
  function navigate(event:MouseEvent<HTMLAnchorElement>) {
    onClick?.(event);
    if (event.defaultPrevented || !internal || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    router.push(href);
  }
  return <a {...props} href={internal ? '#' + href : href} onClick={navigate}>{children}</a>;
}
