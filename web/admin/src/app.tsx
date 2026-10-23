/** Composes guarded admin workflows from UI primitives and domain clients.
 * Depends on Router/session/catalog/orders; production excludes galleries and fixture labels. */
import {Component,lazy,Suspense,useEffect,useMemo,useRef,useState,type FormEvent,type ReactNode} from 'react';
import {Link,Navigate,Outlet,Route,Routes,useLocation,useNavigate,useParams} from 'react-router';
import {Button,EmptyState,FormField,TextInput} from '@opensite/ui';
import {APIError,CatalogAPI} from './api';
import {validateUUID} from './validation';
import {SessionProvider,useSession} from './session';
import {ProductCreatePage,ProductEditPage,ProductListPage} from './products';
import {OrdersAPI,OrderDetailPage,OrderListPage} from './orders';
// Native selects cache text metrics at creation; waiting before screenshots is too late.
// Keep the font boundary inside the build-guarded gallery, before any controls mount.
const Gallery=import.meta.env.DEV||import.meta.env.MODE==='gallery'?lazy(async()=>{
 const gallery=await import('./gallery');
 const fontSize=getComputedStyle(document.body).fontSize;
 await Promise.all(['400','500','600'].map(weight=>document.fonts.load(`${weight} ${fontSize} Inter`, 'Default Selected')));
 return gallery;
}):null;
// Contract fixtures are supplied only by the isolated test runner's interception.
const ordersFixture=import.meta.env.DEV||import.meta.env.MODE==='gallery';
export class ErrorBoundary extends Component<{children:ReactNode},{failed:boolean}>{
 state={failed:false};static getDerivedStateFromError(){return {failed:true};}
 render(){return this.state.failed?<main id="main" className="admin-page"><EmptyState headingLevel={1} title="This page could not be displayed" description="Reload to try again. Your saved changes are safe." action={<Button onClick={()=>window.location.reload()}>Reload page</Button>}/></main>:this.props.children;}
}
function FocusRoute(){const location=useLocation();useEffect(()=>{document.title='OpenSite admin';let done=false;const focus=()=>{const h=document.querySelector<HTMLElement>('main h1');if(h&&!done){h.tabIndex=-1;h.focus();done=true;}};focus();const observer=new MutationObserver(focus);observer.observe(document.getElementById('root')!,{childList:true,subtree:true});return()=>observer.disconnect();},[location.pathname]);return null;}
function Guard(){const {session,loading,error,refresh}=useSession();const location=useLocation();
 if(loading)return <main id="main" className="admin-page"><p role="status">Checking session…</p></main>;
 if(error)return <main id="main" className="admin-page"><p role="alert">{error}</p><Button onClick={refresh}>Retry session</Button></main>;
 return session?<Outlet/>:<Navigate to="/login" state={{returnTo:location.pathname+location.search}} replace/>;
}
function Login(){const {session,loading,expired,signIn}=useSession();const location=useLocation();const navigate=useNavigate();
 const [email,setEmail]=useState(''),[password,setPassword]=useState(''),[busy,setBusy]=useState(false),[error,setError]=useState('');const errorRef=useRef<HTMLParagraphElement>(null);
 const wanted=(location.state as {returnTo?:string}|null)?.returnTo;const destination=wanted&&/^\/t\/[0-9a-f-]+\//i.test(wanted)?wanted:'/';
 useEffect(()=>{if(error)errorRef.current?.focus();},[error]);
 if(loading)return <main id="main" className="admin-page"><p role="status">Checking session…</p></main>;
 if(session)return <Navigate to={destination} replace/>;
 async function submit(event:FormEvent){event.preventDefault();setBusy(true);setError('');try{await signIn(email,password);setPassword('');navigate(destination,{replace:true});}catch(reason){setPassword('');setError(reason instanceof APIError&&reason.status===429?'Sign-in is temporarily limited. Wait and try again.':'Unable to sign in. Check your credentials or try again later.');}finally{setBusy(false);}}
 return <main id="main" className="admin-login"><div className="admin-login-intro"><Link to="/">OpenSite admin</Link><h1 tabIndex={-1}>Sign in</h1><p>Manage your products and orders.</p></div><form className="admin-panel admin-form" onSubmit={submit}>
 {expired&&<p role="status">Your session expired. Sign in to continue.</p>}{error&&<p className="admin-error" role="alert" ref={errorRef} tabIndex={-1}>{error}</p>}
 <FormField id="login-email" label="Email"><TextInput type="email" autoComplete="username" required maxLength={254} value={email} onChange={e=>setEmail(e.target.value)} disabled={busy}/></FormField>
 <FormField id="login-password" label="Password"><TextInput type="password" autoComplete="current-password" required maxLength={1024} value={password} onChange={e=>setPassword(e.target.value)} disabled={busy}/></FormField>
 <Button type="submit" loading={busy}>Sign in</Button></form></main>;
}
function ChooseStore(){const [tenant,setTenant]=useState(''),[error,setError]=useState('');const navigate=useNavigate();return <main id="main" className="admin-page"><h1 tabIndex={-1}>Open a store</h1><p>Your store ID is supplied by your administrator. Access is checked by the server.</p><form className="admin-panel admin-form" onSubmit={e=>{e.preventDefault();if(!validateUUID(tenant)){setError('Enter a valid store ID.');return;}navigate(`/t/${tenant}/products`);}}><FormField id="tenant-id" label="Store ID" error={error}><TextInput required value={tenant} onChange={e=>setTenant(e.target.value.trim())}/></FormField><Button type="submit">Open store</Button></form></main>;}
function Frame(){const {tenantId=''}=useParams();const {signOut}=useSession();const [error,setError]=useState(''),[busy,setBusy]=useState(false);
 return <div className="admin-shell"><a className="admin-skip" href="#main">Skip to content</a><header className="admin-topbar"><Link to="/" className="admin-brand">OpenSite admin</Link><div className="admin-actions"><Link to="/">Switch store</Link><Button variant="ghost" loading={busy} onClick={()=>{setBusy(true);signOut().catch(()=>setError('Unable to sign out. Retry to revoke your session.')).finally(()=>setBusy(false));}}>Sign out</Button></div></header>{error&&<p role="alert" className="admin-error">{error}</p>}
 <div className="admin-workspace">{tenantId&&<nav className="admin-nav" aria-label="Store navigation"><Link to={`/t/${tenantId}/products`}>Products</Link><Link to={`/t/${tenantId}/orders`}>Orders</Link></nav>}<Outlet/></div></div>;
}
function DomainPage({kind}:{kind:'list'|'create'|'edit'|'orders'|'order'}){const {api}=useSession();const {tenantId='',productId='',orderId=''}=useParams();const valid=validateUUID(tenantId);
 const catalog=useMemo(()=>valid?new CatalogAPI(api,tenantId):null,[api,tenantId,valid]);const orders=useMemo(()=>valid?new OrdersAPI(api):null,[api,tenantId,valid]);
 if(!catalog||!orders)return <main id="main" className="admin-page"><EmptyState headingLevel={1} title="Store unavailable" description="Check the store link with your administrator."/></main>;
 if(kind==='orders'||kind==='order')return <main id="main" key={tenantId+orderId}>{kind==='orders'?<OrderListPage api={orders} tenantId={tenantId} fixture={ordersFixture}/>:<OrderDetailPage api={orders} tenantId={tenantId} fixture={ordersFixture}/>}</main>;
 return <main id="main" key={tenantId+productId+kind}>{kind==='list'?<ProductListPage api={catalog} tenantId={tenantId}/>:kind==='create'?<ProductCreatePage api={catalog} tenantId={tenantId}/>:<ProductEditPage api={catalog} tenantId={tenantId}/>}</main>;
}
export function App(){return <ErrorBoundary><SessionProvider><FocusRoute/><Suspense fallback={<p role="status">Loading…</p>}><Routes>
 {Gallery&&<Route path="/_gallery" element={<Gallery/>}/>}
 <Route path="/login" element={<Login/>}/><Route element={<Guard/>}><Route element={<Frame/>}><Route path="/" element={<ChooseStore/>}/><Route path="/t/:tenantId/products" element={<DomainPage kind="list"/>}/><Route path="/t/:tenantId/products/new" element={<DomainPage kind="create"/>}/><Route path="/t/:tenantId/products/:productId" element={<DomainPage kind="edit"/>}/><Route path="/t/:tenantId/orders" element={<DomainPage kind="orders"/>}/><Route path="/t/:tenantId/orders/:orderId" element={<DomainPage kind="order"/>}/></Route></Route>
 <Route path="*" element={<Navigate to="/" replace/>}/></Routes></Suspense></SessionProvider></ErrorBoundary>;}
