/** Owns ephemeral staff session state; depends on PR43 transport and Query cache.
 * Admin routes consume it; HttpOnly cookies never enter JS/storage and 401 clears private views. */
import {createContext,useContext,useEffect,useRef,useState,type ReactNode} from 'react';
import {useQueryClient} from '@tanstack/react-query';
import {AdminAPI,APIError,type AdminSessionView} from './api';
import {scheduleAt} from './validation';
type SessionState={api:AdminAPI;session:AdminSessionView|null;loading:boolean;expired:boolean;error:string;refresh:()=>void;signIn:(email:string,password:string)=>Promise<void>;signOut:()=>Promise<void>};
const SessionContext=createContext<SessionState|null>(null);
export function SessionProvider({children}:{children:ReactNode}){
 const cache=useQueryClient();const [session,setSession]=useState<AdminSessionView|null>(null);
 const [loading,setLoading]=useState(true),[expired,setExpired]=useState(false),[error,setError]=useState(''),[revision,setRevision]=useState(0);
 const epoch=useRef(0),bootstrap=useRef<AbortController|null>(null);
 const [api]=useState(()=>new AdminAPI({onSessionExpired:()=>{epoch.current++;setSession(null);setExpired(true);cache.clear();}}));
 useEffect(()=>{const abort=new AbortController(),current=++epoch.current;bootstrap.current=abort;setLoading(true);setError('');
  api.session(abort.signal).then(view=>{if(current===epoch.current)setSession(view);}).catch(reason=>{if(abort.signal.aborted)return;if(reason instanceof APIError&&reason.status===401){setSession(null);return;}setError('Unable to check your session. Try again.');}).finally(()=>{if(!abort.signal.aborted)setLoading(false);});return()=>abort.abort();
 },[api,revision]);
 useEffect(()=>{if(!session)return;return scheduleAt(Date.parse(session.expires_at),()=>{api.clearSession();epoch.current++;setSession(null);setExpired(true);cache.clear();});},[session,api,cache]);
 async function signIn(email:string,password:string){bootstrap.current?.abort();epoch.current++;const view=await api.login({email,password});cache.clear();setSession(view);setLoading(false);setExpired(false);setError('');}
 async function signOut(){await api.logout();epoch.current++;cache.clear();setSession(null);setExpired(false);}
 return <SessionContext.Provider value={{api,session,loading,expired,error,refresh:()=>setRevision(v=>v+1),signIn,signOut}}>{children}</SessionContext.Provider>;
}
export function useSession(){const value=useContext(SessionContext);if(!value)throw new Error('Session provider missing');return value;}
