pub mod tunnel;

use std::collections::HashMap;

#[derive(Default)]
pub struct ServiceConfig {
    entries: HashMap<String, u16>,
}

pub struct RequestTarget<'a> {
    service: &'a str,
    path: &'a str,
}

impl ServiceConfig {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn set(&mut self, value: &str) -> Result<(), String> {
        let parts = value.split_once('=');
        
        match parts {
            Some((name , port)) => {
                let name = name.trim();
                if name.is_empty(){
                    return Err("service name is empty".to_owned());
                }
                let port = port.trim();
            
                let port : u16 = port.parse().map_err(|_| format!("invalid port: {port}"))?;

                if port == 0 {
                    return Err(format!("invalid port: {port}"));
                }

                self.entries.insert(name.to_owned(), port);
                Ok(())
            }
            None => {
                return Err("service must be in name=port format".to_owned());
            }
        }
    }

    pub fn get_port(&self, service: &str) -> Option<u16>{
        self.entries.get(service).copied()
    }

    //function to generate a url from a path
    //"localhost:9000/raise-up/dog/12345" (being the path)
    pub fn generate_url(&self, target: RequestTarget<'_>) -> Result<String, String>{
        if target.path.is_empty(){
            return Err("Path cannot be empty string".to_owned())
        }
        let port = match self.get_port(target.service){
            Some(port) => port,
            None => return Err("Service not found".to_owned())
        };

        Ok(format!("http://127.0.0.1:{port}{}", target.path))
    }
}
